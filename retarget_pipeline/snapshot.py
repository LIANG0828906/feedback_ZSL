"""工程文件、动画快照、哈希链与崩溃恢复。

落盘策略
--------
* 所有写入走“临时文件 + ``os.replace`` 原子重命名 + ``fsync``”，
  进程被强制终止时磁盘上只可能出现上一次完整提交或本次完整提交，
  绝不会出现半写文件；
* 每个快照带内容哈希，且 ``prev_hash`` 指向上一快照，形成哈希链；
* 恢复时逐链校验版本与哈希，断链以
  :data:`~retarget_pipeline.errors.E_RECOVERY_INVALID` 中止；
* 映射哈希或格式版本变化会被识别，返回 ``needs_recompute`` 触发重算。
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass

import numpy as np

from .errors import E_RECOVERY_INVALID, PipelineError
from .model import RetargetResult

SNAPSHOT_PREFIX = "snapshot_"
SNAPSHOT_SUFFIX = ".json"
PROJECT_FILE = "project.json"


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def canonical_json_bytes(payload: dict) -> bytes:
    return json.dumps(payload, ensure_ascii=False, sort_keys=True,
                      indent=2, separators=(",", ": ")).encode("utf-8")


def atomic_write(path: str, payload: dict) -> str:
    """原子写 JSON，返回该内容的 sha256。"""

    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    data = canonical_json_bytes(payload)
    fd, tmp_path = tempfile.mkstemp(prefix=".tmp_snap_", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)
    except BaseException:
        if os.path.exists(tmp_path):
            os.unlink(tmp_path)
        raise
    return _sha256_bytes(data)


@dataclass
class ProjectState:
    project_dir: str
    source_version: int
    target_version: int
    mapping_hash: str
    last_snapshot: str | None
    last_committed_frame: int

    def to_dict(self) -> dict:
        return {
            "schema": "retarget-project/1",
            "source_version": self.source_version,
            "target_version": self.target_version,
            "mapping_hash": self.mapping_hash,
            "last_snapshot": self.last_snapshot,
            "last_committed_frame": self.last_committed_frame,
        }


def init_project(project_dir: str, source_version: int, target_version: int,
                 mapping_hash: str) -> ProjectState:
    os.makedirs(project_dir, exist_ok=True)
    state = ProjectState(
        project_dir=project_dir,
        source_version=source_version,
        target_version=target_version,
        mapping_hash=mapping_hash,
        last_snapshot=None,
        last_committed_frame=-1,
    )
    atomic_write(os.path.join(project_dir, PROJECT_FILE), state.to_dict())
    return state


def _frame_payload(frame) -> dict:
    return {
        "frame": int(frame.frame),
        "resolved": bool(frame.resolved),
        "error_code": frame.error_code,
        "global_rotations": np.asarray(frame.global_rotations).tolist(),
        "local_rotations": np.asarray(frame.local_rotations).tolist(),
        "root_translation": np.asarray(frame.root_translation).tolist(),
    }


def commit_frame(project_dir: str, state: ProjectState, frame,
                 mapping_hash: str, source_version: int,
                 target_version: int) -> tuple[ProjectState, str]:
    """提交单帧快照并推进哈希链。"""

    seq = state.last_committed_frame + 1
    snapshot_name = f"{SNAPSHOT_PREFIX}{seq:06d}{SNAPSHOT_SUFFIX}"
    body = {
        "schema": "retarget-snapshot/1",
        "sequence": seq,
        "mapping_hash": mapping_hash,
        "source_version": source_version,
        "target_version": target_version,
        "prev_hash": state.last_snapshot,
        "frame": _frame_payload(frame),
    }
    # 内容哈希不包含自身 hash 字段，保证可复算。
    content_hash = atomic_write(os.path.join(project_dir, snapshot_name), body)
    body["content_hash"] = content_hash
    # 再写一次带 hash 的同名文件（仍是原子替换），恢复时复算 body 去掉 hash 即可。
    atomic_write(os.path.join(project_dir, snapshot_name), body)

    state.last_snapshot = content_hash
    state.last_committed_frame = seq
    state.mapping_hash = mapping_hash
    state.source_version = source_version
    state.target_version = target_version
    atomic_write(os.path.join(project_dir, PROJECT_FILE), state.to_dict())
    return state, content_hash


def _snapshot_sequence(filename: str) -> int:
    stem = filename[len(SNAPSHOT_PREFIX):-len(SNAPSHOT_SUFFIX)]
    return int(stem)


def list_snapshots(project_dir: str) -> list[str]:
    if not os.path.isdir(project_dir):
        return []
    files = [
        f for f in os.listdir(project_dir)
        if f.startswith(SNAPSHOT_PREFIX) and f.endswith(SNAPSHOT_SUFFIX)
    ]
    return sorted(files, key=_snapshot_sequence)


@dataclass
class RecoveryResult:
    state: ProjectState
    frames: list[dict]
    needs_recompute: bool
    recompute_reason: str | None


def recover(project_dir: str, *, current_mapping_hash: str | None = None,
            current_source_version: int | None = None,
            current_target_version: int | None = None) -> RecoveryResult:
    """仅依据磁盘快照恢复到最后一次已提交帧，并校验哈希链。"""

    project_path = os.path.join(project_dir, PROJECT_FILE)
    if not os.path.exists(project_path):
        raise PipelineError(E_RECOVERY_INVALID, f"工程文件缺失: {project_path}",
                            [{"field": PROJECT_FILE}])
    try:
        with open(project_path, "r", encoding="utf-8") as handle:
            project = json.load(handle)
    except json.JSONDecodeError as exc:
        raise PipelineError(E_RECOVERY_INVALID, f"工程文件损坏: {exc.msg}",
                            [{"field": PROJECT_FILE}])

    state = ProjectState(
        project_dir=project_dir,
        source_version=int(project["source_version"]),
        target_version=int(project["target_version"]),
        mapping_hash=project["mapping_hash"],
        last_snapshot=project.get("last_snapshot"),
        last_committed_frame=int(project["last_committed_frame"]),
    )

    snapshots = list_snapshots(project_dir)
    frames: list[dict] = []
    prev_hash: str | None = None
    for name in snapshots:
        path = os.path.join(project_dir, name)
        try:
            with open(path, "r", encoding="utf-8") as handle:
                snap = json.load(handle)
        except json.JSONDecodeError as exc:
            raise PipelineError(E_RECOVERY_INVALID, f"快照损坏 {name}: {exc.msg}",
                                [{"field": name}])
        stored_hash = snap.pop("content_hash", None)
        recomputed = _sha256_bytes(canonical_json_bytes(snap))
        if stored_hash != recomputed:
            raise PipelineError(
                E_RECOVERY_INVALID,
                f"快照内容哈希不匹配: {name}",
                [{"field": "content_hash", "snapshot": name,
                  "expected": stored_hash, "recomputed": recomputed}],
            )
        if snap.get("prev_hash") != prev_hash:
            raise PipelineError(
                E_RECOVERY_INVALID,
                f"快照哈希链断裂: {name}",
                [{"field": "prev_hash", "snapshot": name,
                  "expected_prev": prev_hash, "actual_prev": snap.get("prev_hash")}],
            )
        prev_hash = stored_hash
        frames.append(snap["frame"])

    if state.last_snapshot is not None and state.last_snapshot != prev_hash:
        raise PipelineError(
            E_RECOVERY_INVALID,
            "工程指针与快照链尾不一致",
            [{"field": "last_snapshot", "project_pointer": state.last_snapshot,
              "chain_tail": prev_hash}],
        )

    needs_recompute = False
    reason = None
    if current_mapping_hash is not None and current_mapping_hash != state.mapping_hash:
        needs_recompute = True
        reason = "mapping_hash_changed"
    elif current_source_version is not None and current_source_version != state.source_version:
        needs_recompute = True
        reason = "source_version_changed"
    elif current_target_version is not None and current_target_version != state.target_version:
        needs_recompute = True
        reason = "target_version_changed"

    return RecoveryResult(state=state, frames=frames, needs_recompute=needs_recompute,
                          recompute_reason=reason)


def result_from_recovered_frames(target_names: list[str], frames: list[dict],
                                 mapping_hash: str, source_version: int,
                                 target_version: int) -> RetargetResult:
    """把恢复出的帧字典重建为 :class:`RetargetResult`。"""

    from .model import RetargetFrame

    rebuilt = []
    for item in frames:
        rebuilt.append(RetargetFrame(
            frame=int(item["frame"]),
            global_rotations=np.asarray(item["global_rotations"], dtype=np.float64),
            local_rotations=np.asarray(item["local_rotations"], dtype=np.float64),
            root_translation=np.asarray(item["root_translation"], dtype=np.float64),
            resolved=bool(item["resolved"]),
            error_code=item.get("error_code"),
        ))
    return RetargetResult(
        target_names=list(target_names),
        frames=rebuilt,
        mapping_hash=mapping_hash,
        source_version=source_version,
        target_version=target_version,
    )
