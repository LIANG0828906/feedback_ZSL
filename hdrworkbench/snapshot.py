"""强制终止后的分块快照与恢复。

快照目录布局::

    snapshot_dir/
      manifest.json        参数集 + 版本元数据 + 链头（原子发布）
      chunks/chunk_XXXXXX.tmp / chunk_XXXXXX   每帧一个分块

每个分块含帧号、帧内容哈希、前一块哈希（链式）、编码码值与误差摘要。
提交顺序固定：写临时分块 -> fsync -> 原子重命名；全部完成后再发布
manifest。恢复时逐块校验哈希链，断链 / 字段缺失立即 E_RECOVERY_INVALID
并定位首个断链帧；已提交帧绝不重复写入，只补算缺失帧。
"""

from __future__ import annotations

import os

from .errors import RecoveryInvalidError
from .hashing import array_hash, dict_hash, stable_json

GENESIS = "0" * 64


class CrashInjectedError(RuntimeError):
    """测试用：模拟强制终止，manifest 尚未发布因此下次进入恢复流程。"""


def _chunk_path(chunk_dir: str, frame_index: int) -> str:
    return os.path.join(chunk_dir, f"chunk_{frame_index:06d}")


def _write_chunk(chunk: dict, chunk_dir: str) -> None:
    path = _chunk_path(chunk_dir, chunk["frame_index"])
    tmp_path = path + ".tmp"
    payload = stable_json(chunk)
    with open(tmp_path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp_path, path)


def _read_chunk(path: str) -> dict:
    import json

    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def _validate_chunk_fields(chunk: dict, frame_index: int) -> None:
    required = {
        "frame_index",
        "content_hash",
        "prev_hash",
        "codes",
        "bound_summary",
        "source_hash",
    }
    missing = required.difference(chunk)
    if missing:
        raise RecoveryInvalidError(
            f"快照分块字段缺失: {sorted(missing)}",
            bad_frame=frame_index,
            reason="missing_field",
        )


class SnapshotWriter:
    """按帧提交快照；可注入崩溃点验证恢复。"""

    def __init__(
        self,
        snapshot_dir: str,
        manifest: dict,
        crash_after: int | None = None,
    ):
        self.snapshot_dir = snapshot_dir
        self.chunk_dir = os.path.join(snapshot_dir, "chunks")
        os.makedirs(self.chunk_dir, exist_ok=True)
        self.manifest = manifest
        self.crash_after = crash_after
        self._last_hash = GENESIS
        self._committed = self._scan_committed()
        if self._committed:
            self._verify_chain(self._committed)
            if manifest:
                self._verify_factors(manifest)
            self._last_hash = self._read_committed(self._committed[-1])[
                "content_chain_hash"
            ]

    def _verify_factors(self, manifest: dict) -> None:
        """恢复时校验已提交快照的参数集与当前请求一致，否则禁止复用。"""
        from .hashing import dict_hash as _dict_hash

        if "factors" not in manifest:
            raise RecoveryInvalidError(
                "快照 manifest 缺少参数集",
                bad_frame=self._committed[0],
                reason="missing_factors",
            )
        first = self._read_committed(self._committed[0])
        if first.get("factor_fingerprint") != _dict_hash(manifest["factors"]):
            raise RecoveryInvalidError(
                "快照参数集与当前请求不一致，必须全量重算",
                bad_frame=self._committed[0],
                reason="factor_mismatch",
            )

    def _scan_committed(self) -> list[int]:
        if not os.path.isdir(self.chunk_dir):
            return []
        indices = []
        for name in os.listdir(self.chunk_dir):
            if name.startswith("chunk_") and not name.endswith(".tmp"):
                indices.append(int(name.split("_")[1]))
        return sorted(indices)

    def _read_committed(self, frame_index: int) -> dict:
        chunk = _read_chunk(_chunk_path(self.chunk_dir, frame_index))
        _validate_chunk_fields(chunk, frame_index)
        return chunk

    def _verify_chain(self, indices: list[int]) -> None:
        expected_prev = GENESIS
        for position, frame_index in enumerate(indices):
            if position != frame_index:
                raise RecoveryInvalidError(
                    "快照分块序列不连续",
                    bad_frame=frame_index,
                    reason="non_contiguous",
                )
            chunk = self._read_committed(frame_index)
            if chunk["prev_hash"] != expected_prev:
                raise RecoveryInvalidError(
                    "快照哈希链断裂",
                    bad_frame=frame_index,
                    reason="broken_chain",
                )
            expected_prev = chunk["content_chain_hash"]

    @property
    def committed_frames(self) -> list[int]:
        return list(self._committed)

    def write_frame(
        self,
        frame_index: int,
        codes,
        bound_summary: dict,
        source_hash: str,
        stage_hashes: dict,
    ) -> dict:
        if frame_index in self._committed:
            # 已提交帧不重复写入，直接返回既有分块
            return self._read_committed(frame_index)
        content_hash = array_hash(codes)
        chain_hash = dict_hash(
            {
                "prev": self._last_hash,
                "frame": frame_index,
                "content": content_hash,
            }
        )
        chunk = {
            "frame_index": frame_index,
            "content_hash": content_hash,
            "prev_hash": self._last_hash,
            "content_chain_hash": chain_hash,
            "codes": codes.astype("uint16" if codes.dtype != "uint8" else "uint8")
            .tolist(),
            "bound_summary": bound_summary,
            "source_hash": source_hash,
            "stage_hashes": stage_hashes,
            "factor_fingerprint": dict_hash(self.manifest.get("factors", {})),
        }
        _write_chunk(chunk, self.chunk_dir)
        self._committed.append(frame_index)
        self._last_hash = chain_hash
        if self.crash_after is not None and frame_index >= self.crash_after:
            raise CrashInjectedError(
                f"模拟强制终止：已提交到帧 {frame_index}，manifest 未发布"
            )
        return chunk

    def publish_manifest(self) -> None:
        from .versions import atomic_write_json

        payload = dict(self.manifest)
        payload["committed_frames"] = self._committed
        payload["chain_head"] = self._last_hash
        atomic_write_json(
            os.path.join(self.snapshot_dir, "manifest.json"),
            stable_json(payload),
        )


def recover_snapshot(snapshot_dir: str) -> dict:
    """恢复入口：校验链并返回已提交帧与待补算帧集合。"""
    import json

    chunk_dir = os.path.join(snapshot_dir, "chunks")
    manifest_path = os.path.join(snapshot_dir, "manifest.json")
    if not os.path.isdir(chunk_dir):
        raise RecoveryInvalidError(
            "快照目录缺失", bad_frame=-1, reason="no_snapshot_dir"
        )
    writer = SnapshotWriter(snapshot_dir, manifest={})
    committed = writer.committed_frames
    writer._verify_chain(committed)
    manifest = None
    if os.path.exists(manifest_path):
        with open(manifest_path, "r", encoding="utf-8") as handle:
            manifest = json.load(handle)
        for field_name in ("committed_frames", "chain_head"):
            if field_name not in manifest:
                raise RecoveryInvalidError(
                    f"manifest 字段缺失: {field_name}",
                    bad_frame=-1,
                    reason="missing_manifest_field",
                )
        if manifest["committed_frames"] != committed:
            raise RecoveryInvalidError(
                "manifest 已提交帧清单与分块不一致",
                bad_frame=committed[-1] if committed else -1,
                reason="manifest_drift",
            )
        if manifest["chain_head"] != writer._last_hash:
            raise RecoveryInvalidError(
                "manifest 链头与分块哈希链不一致",
                bad_frame=committed[-1] if committed else -1,
                reason="chain_head_drift",
            )
    frames = {
        index: writer._read_committed(index) for index in committed
    }
    return {
        "committed_frames": committed,
        "frames": frames,
        "manifest": manifest,
        "chain_head": writer._last_hash,
    }

