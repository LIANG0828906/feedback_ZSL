"""v1 / v2 / v3 骨骼动画格式的解析与序列化。

文件布局（UTF-8 JSON，顶层带 ``format_version``）::

    v1: format_version, frame_rate, joints[ {name} ],
        frames[ {rotations: [[w,x,y,z] * N]} ]
    v2: 额外 joints[].parent（索引，根为 -1）、frames[].translations[[x,y,z]*N]
    v3: 额外 joints[].bind_matrix（4x4）、joints[].scale、joints[].aliases

所有失败统一抛 :class:`~retarget_pipeline.errors.PipelineError`，
业务码 :data:`~retarget_pipeline.errors.E_ANIM_FORMAT_INVALID`，
并在 ``locations`` 中给出 joint / field / frame 定位；校验失败不产出部分文档。
"""

from __future__ import annotations

import json
import os
import tempfile
from typing import Any

import numpy as np

from .errors import E_ANIM_FORMAT_INVALID, PipelineError, raise_format
from .math3d import order_hierarchy, quat_is_degenerate
from .model import SUPPORTED_VERSIONS, AnimDocument, JointDef


# ---------------------------------------------------------------- 读取
def load_file(path: str) -> AnimDocument:
    """从磁盘读取并解析动画文件（文件不存在按格式错误上报）。"""

    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError:
        raise PipelineError(E_ANIM_FORMAT_INVALID, f"动画文件不存在: {path}",
                            [{"field": "file", "value": path}])
    except json.JSONDecodeError as exc:
        raise PipelineError(
            E_ANIM_FORMAT_INVALID,
            f"JSON 解析失败: {exc.msg}",
            [{"field": "json", "line": exc.lineno, "column": exc.colno}],
        )
    doc = parse_document(raw)
    doc.source_path = path
    return doc


def parse_document(raw: Any) -> AnimDocument:
    """解析已加载的 JSON 字典为 :class:`AnimDocument`，全量校验后才返回。"""

    if not isinstance(raw, dict):
        raise_format("顶层结构必须是 JSON 对象", field_name="root")
    version = raw.get("format_version")
    if version not in SUPPORTED_VERSIONS:
        raise_format(
            f"未知格式版本号: {version!r}，支持 {SUPPORTED_VERSIONS}",
            field_name="format_version",
            extra={"value": version},
        )

    frame_rate = float(raw.get("frame_rate", 30.0))
    if frame_rate <= 0:
        raise_format("frame_rate 必须为正数", field_name="frame_rate", extra={"value": frame_rate})

    joints_raw = raw.get("joints")
    if not isinstance(joints_raw, list) or not joints_raw:
        raise_format("joints 必须是非空数组", field_name="joints")

    frames_raw = raw.get("frames")
    if not isinstance(frames_raw, list) or not frames_raw:
        raise_format("frames 必须是非空数组", field_name="frames")

    n_joints = len(joints_raw)
    joints: list[JointDef] = []
    seen_names: set[str] = set()
    parents: list[int | None] = []

    for idx, item in enumerate(joints_raw):
        if not isinstance(item, dict):
            raise_format("关节条目必须是对象", joint=f"#{idx}", field_name="joint")
        name = item.get("name")
        if not isinstance(name, str) or not name:
            raise_format("关节缺少有效 name", joint=f"#{idx}", field_name="name")
        if name in seen_names:
            raise_format("关节名重复", joint=name, field_name="name")
        seen_names.add(name)

        parent: int | None = None
        if version >= 2:
            if "parent" not in item:
                raise_format("v2+ 关节缺少 parent 字段", joint=name, field_name="parent")
            parent = _validate_parent(item["parent"], name, n_joints)
        parents.append(parent)

        bind_matrix = None
        scale = 1.0
        aliases: list[str] = []
        if version >= 3:
            bind_matrix = _validate_bind_matrix(item.get("bind_matrix"), name)
            scale = _validate_scale(item.get("scale", 1.0), name)
            aliases = _validate_aliases(item.get("aliases", []), name)

        joints.append(JointDef(name=name, parent=parent, bind_matrix=bind_matrix,
                               scale=scale, aliases=aliases))

    # v1 无层级：首关节为根，其余挂到前一个关节（线性链），并在 provenance 标注。
    hierarchy_derived = False
    if version == 1:
        hierarchy_derived = True
        for idx in range(len(joints)):
            joints[idx].parent = None if idx == 0 else idx - 1
        parents = [j.parent for j in joints]

    # 成环 / 越域统一在此暴露（v2+ 已做索引范围检查，这里再做拓扑校验）。
    try:
        order_hierarchy(parents)
    except ValueError as exc:
        raise_format(str(exc), field_name="parent")

    rotations = np.zeros((len(frames_raw), n_joints, 4), dtype=np.float64)
    translations = np.zeros((len(frames_raw), n_joints, 3), dtype=np.float64)

    for f_idx, frame in enumerate(frames_raw):
        if not isinstance(frame, dict):
            raise_format("帧条目必须是对象", field_name="frame", frame=f_idx)
        rot = frame.get("rotations")
        if not isinstance(rot, list) or len(rot) != n_joints:
            raise_format("rotations 数量必须与关节数一致", field_name="rotations", frame=f_idx)
        for j_idx, quat in enumerate(rot):
            rotations[f_idx, j_idx] = _validate_quat(quat, joints[j_idx].name, f_idx)

        if version >= 2:
            if "translations" not in frame:
                raise_format("v2+ 帧缺少 translations", field_name="translations", frame=f_idx)
            trans = frame["translations"]
            if not isinstance(trans, list) or len(trans) != n_joints:
                raise_format("translations 数量必须与关节数一致",
                             field_name="translations", frame=f_idx)
            for j_idx, vec in enumerate(trans):
                translations[f_idx, j_idx] = _validate_vec3(vec, joints[j_idx].name, f_idx)

    doc = AnimDocument(
        version=version,
        joints=joints,
        local_rotations=rotations,
        local_translations=translations,
        frame_rate=frame_rate,
        metadata={"format_version": str(version)},
    )
    if hierarchy_derived:
        for joint in joints:
            doc.provenance[joint.name] = {"parent": "derived:v1_linear_chain"}
    return doc


# ---------------------------------------------------------------- 写入
def to_json_dict(doc: AnimDocument, version: int | None = None) -> dict[str, Any]:
    """把文档序列化为可 JSON 化的字典；``version`` 用于显式降级/升级导出。"""

    target_version = doc.version if version is None else version
    if target_version not in SUPPORTED_VERSIONS:
        raise_format(f"未知导出版本号: {target_version}", field_name="format_version")

    n = doc.joint_count
    if doc.local_rotations.shape[1] != n:
        raise_format("旋转通道与关节数不一致", field_name="rotations")

    joints_out: list[dict[str, Any]] = []
    for idx, joint in enumerate(doc.joints):
        entry: dict[str, Any] = {"name": joint.name}
        if target_version >= 2:
            entry["parent"] = -1 if joint.parent is None else joint.parent
        if target_version >= 3:
            bind = np.eye(4) if joint.bind_matrix is None else joint.bind_matrix
            entry["bind_matrix"] = _round_matrix(bind)
            entry["scale"] = float(joint.scale)
            entry["aliases"] = list(joint.aliases)
        joints_out.append(entry)

    frames_out: list[dict[str, Any]] = []
    for f_idx in range(doc.frame_count):
        frame: dict[str, Any] = {
            "rotations": [[float(x) for x in doc.local_rotations[f_idx, j]] for j in range(n)]
        }
        if target_version >= 2:
            frame["translations"] = [
                [float(x) for x in doc.local_translations[f_idx, j]] for j in range(n)
            ]
        frames_out.append(frame)

    return {
        "format_version": target_version,
        "frame_rate": float(doc.frame_rate),
        "joints": joints_out,
        "frames": frames_out,
    }


def dump_file(doc: AnimDocument, path: str, version: int | None = None) -> None:
    """原子写盘：先写临时文件再 ``os.replace``，避免半写文件。"""

    payload = to_json_dict(doc, version)
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    fd, tmp_path = tempfile.mkstemp(prefix=".tmp_anim_", suffix=".json", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, sort_keys=True, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)
    except BaseException:
        if os.path.exists(tmp_path):
            os.unlink(tmp_path)
        raise


def canonical_bytes(doc: AnimDocument, version: int | None = None) -> bytes:
    """规范化字节序列，供内容哈希与逐字节一致性比对。"""

    return json.dumps(
        to_json_dict(doc, version), ensure_ascii=False, sort_keys=True, indent=2
    ).encode("utf-8") + b"\n"


# ---------------------------------------------------------------- 校验
def _validate_parent(value: Any, joint: str, n_joints: int) -> int | None:
    if not isinstance(value, int) or isinstance(value, bool):
        raise_format("parent 必须是整数索引（根为 -1）", joint=joint, field_name="parent",
                     extra={"value": value})
    if value == -1:
        return None
    if value < 0 or value >= n_joints:
        raise_format("parent 索引越域", joint=joint, field_name="parent", extra={"value": value})
    return value


def _validate_quat(value: Any, joint: str, frame: int) -> np.ndarray:
    if not isinstance(value, list) or len(value) != 4 or not all(isinstance(x, (int, float)) for x in value):
        raise_format("旋转必须是长度 4 的数字数组 [w,x,y,z]", joint=joint,
                     field_name="rotations", frame=frame)
    arr = np.asarray(value, dtype=np.float64)
    if not np.all(np.isfinite(arr)):
        raise_format("旋转包含非有限值", joint=joint, field_name="rotations", frame=frame)
    if quat_is_degenerate(arr):
        raise_format("退化（近零）四元数，无法表示旋转", joint=joint,
                     field_name="rotations", frame=frame)
    return arr / float(np.linalg.norm(arr))


def _validate_vec3(value: Any, joint: str, frame: int) -> np.ndarray:
    if not isinstance(value, list) or len(value) != 3 or not all(isinstance(x, (int, float)) for x in value):
        raise_format("平移必须是长度 3 的数字数组 [x,y,z]", joint=joint,
                     field_name="translations", frame=frame)
    arr = np.asarray(value, dtype=np.float64)
    if not np.all(np.isfinite(arr)):
        raise_format("平移包含非有限值", joint=joint, field_name="translations", frame=frame)
    return arr


def _validate_bind_matrix(value: Any, joint: str) -> np.ndarray:
    if value is None:
        raise_format("v3 关节缺少 bind_matrix", joint=joint, field_name="bind_matrix")
    if not isinstance(value, list) or len(value) != 4 or any(
        not isinstance(row, list) or len(row) != 4 for row in value
    ):
        raise_format("bind_matrix 必须是 4x4 数组", joint=joint, field_name="bind_matrix")
    mat = np.asarray(value, dtype=np.float64)
    if not np.all(np.isfinite(mat)):
        raise_format("bind_matrix 包含非有限值", joint=joint, field_name="bind_matrix")
    return mat


def _validate_scale(value: Any, joint: str) -> float:
    if not isinstance(value, (int, float)) or isinstance(value, bool) or not np.isfinite(value):
        raise_format("scale 必须是有限数值", joint=joint, field_name="scale", extra={"value": value})
    if value <= 0:
        raise_format("scale 必须为正数", joint=joint, field_name="scale", extra={"value": value})
    return float(value)


def _validate_aliases(value: Any, joint: str) -> list[str]:
    if not isinstance(value, list) or not all(isinstance(a, str) for a in value):
        raise_format("aliases 必须是字符串数组", joint=joint, field_name="aliases")
    return list(value)


def _round_matrix(mat: np.ndarray) -> list[list[float]]:
    return [[float(x) for x in row] for row in np.asarray(mat, dtype=np.float64)]
