from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .audio import (
    WAV_HEADER_SIZE,
    WavInfo,
    atomic_write_bytes,
    create_wav_skeleton_atomic,
    read_output_block,
    read_wav,
    sha256_bytes,
    sha256_file,
    validate_output_file,
    wav_header,
)
from .constants import DEFAULT_BLOCK_SIZE, SESSION_SCHEMA_VERSION
from .errors import AppError
from .models import (
    ChainConfig,
    Interval,
    validate_intervals,
    validate_sample_rate,
)
from .processors import config_sha256


def _canonical_json(payload: dict[str, Any]) -> bytes:
    return json.dumps(
        payload,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def _with_integrity(payload: dict[str, Any]) -> dict[str, Any]:
    body = {key: value for key, value in payload.items() if key != "integrity_sha256"}
    body["integrity_sha256"] = hashlib.sha256(_canonical_json(body)).hexdigest()
    return body


def _atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent)
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(_canonical_json(payload))
            handle.write(b"\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, path)
        if os.name != "nt":
            try:
                directory_fd = os.open(path.parent, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def _stored_path(path: Path, session_path: Path) -> str:
    try:
        relative = os.path.relpath(path, session_path.parent)
    except ValueError:
        return str(path)
    if relative.startswith(".." + os.sep) or relative == "..":
        return str(path)
    return relative


def _resolved_path(value: str, session_path: Path) -> Path:
    path = Path(value)
    return path if path.is_absolute() else (session_path.parent / path)


def block_payload(samples: np.ndarray) -> bytes:
    return np.ascontiguousarray(samples, dtype=np.int16).astype(
        "<i2", copy=False
    ).tobytes(order="C")


@dataclass
class SessionSnapshot:
    path: Path
    raw: dict[str, Any]
    info: WavInfo
    input_path: Path
    output_path: Path
    cache_dir: Path
    input_samples: np.ndarray
    block_size: int
    chain: ChainConfig
    intervals: list[Interval]

    @property
    def block_count(self) -> int:
        if self.info.frames == 0:
            return 0
        return (self.info.frames + self.block_size - 1) // self.block_size

    @property
    def completed(self) -> dict[str, dict[str, Any]]:
        return self.raw.get("completed_blocks", {})

    @property
    def complete(self) -> bool:
        return bool(self.raw.get("complete", False))

    def block_bounds(self, block_index: int) -> tuple[int, int, int]:
        start = block_index * self.block_size
        frames = min(self.block_size, self.info.frames - start)
        return start, frames, start + frames

    def cache_path(self, block_index: int) -> Path:
        return self.cache_dir / f"{block_index:08d}.pcm"

    def expected_summary(self, block_index: int) -> tuple[str, str]:
        start, frames, _ = self.block_bounds(block_index)
        source = self.input_samples[start : start + frames]
        source_sha = sha256_bytes(block_payload(source))
        cfg_sha = config_sha256(
            chain=self.chain,
            intervals=self.intervals,
            start_frame=start,
            frames=frames,
            sample_rate=self.info.sample_rate,
            channels=self.info.channels,
        )
        return source_sha, cfg_sha


class SessionStore:
    def __init__(self, session_path: str | Path) -> None:
        self.path = Path(session_path)

    @property
    def lock_path(self) -> Path:
        return Path(str(self.path) + ".lock")

    def create(
        self,
        *,
        input_path: str | Path,
        output_path: str | Path,
        block_size: int = DEFAULT_BLOCK_SIZE,
        intervals: Iterable[Interval] = (),
        chain: ChainConfig | None = None,
        force: bool = False,
    ) -> SessionSnapshot:
        input_absolute = Path(input_path).resolve()
        output_absolute = Path(output_path).resolve()
        session_absolute = self.path.resolve()
        if not 128 <= block_size <= 1_048_576:
            raise AppError("E_INVALID_ARGUMENT", "block_size 必须在 128..1048576")
        if not input_absolute.is_file():
            raise AppError("E_INVALID_INPUT", "输入 WAV 不存在", path=str(input_absolute))
        if input_absolute == output_absolute:
            raise AppError("E_INVALID_ARGUMENT", "输出 WAV 不能覆盖输入 WAV")
        if session_absolute in (input_absolute, output_absolute):
            raise AppError("E_INVALID_ARGUMENT", "会话文件路径不能与音频路径相同")
        if not force:
            for target in (session_absolute, output_absolute):
                if target.exists():
                    raise AppError("E_INVALID_ARGUMENT", "目标文件已存在", path=str(target))

        samples, info = read_wav(input_absolute)
        if info.frames <= 0:
            raise AppError("E_INVALID_INPUT", "输入 WAV 不包含音频帧")
        chain_config = chain or ChainConfig()
        interval_list = validate_intervals(intervals, info.frames)
        cache_dir = Path(str(session_absolute) + ".blocks")
        if cache_dir.exists() and force:
            shutil.rmtree(cache_dir)
        cache_dir.mkdir(parents=True, exist_ok=True)

        self.path.parent.mkdir(parents=True, exist_ok=True)
        create_wav_skeleton_atomic(
            output_absolute,
            frames=info.frames,
            sample_rate=info.sample_rate,
            channels=info.channels,
        )
        raw: dict[str, Any] = {
            "schema_version": SESSION_SCHEMA_VERSION,
            "session_format": "voice-music-repair",
            "paths": {
                "input": _stored_path(input_absolute, session_absolute),
                "output": _stored_path(output_absolute, session_absolute),
                "block_cache": _stored_path(cache_dir, session_absolute),
            },
            "audio": {
                "sample_rate": info.sample_rate,
                "channels": info.channels,
                "frames": info.frames,
                "sample_width": 2,
                "input_file_sha256": sha256_file(input_absolute),
            },
            "block_size": block_size,
            "processing_chain": chain_config.to_dict(),
            "intervals": [interval.to_dict() for interval in interval_list],
            "completed_blocks": {},
            "complete": False,
            "output_sha256": None,
        }
        snapshot = SessionSnapshot(
            path=session_absolute,
            raw=raw,
            info=info,
            input_path=input_absolute,
            output_path=output_absolute,
            cache_dir=cache_dir,
            input_samples=samples,
            block_size=block_size,
            chain=chain_config,
            intervals=interval_list,
        )
        self.save(snapshot.raw)
        return self.load()

    def save(self, raw: dict[str, Any]) -> None:
        payload = dict(raw)
        payload["schema_version"] = SESSION_SCHEMA_VERSION
        payload = _with_integrity(payload)
        _atomic_write_json(self.path, payload)

    def load(self, *, verify_input: bool = True) -> SessionSnapshot:
        if not self.path.is_file():
            raise AppError("E_RECOVERY_INVALID", "会话文件不存在", session=str(self.path))
        try:
            payload = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise AppError("E_SESSION_MISMATCH", "会话 JSON 无法解析", reason=str(exc)) from exc
        if not isinstance(payload, dict):
            raise AppError("E_SESSION_MISMATCH", "会话根节点必须是对象")
        supplied_integrity = payload.pop("integrity_sha256", None)
        if not isinstance(supplied_integrity, str):
            raise AppError("E_SESSION_MISMATCH", "会话缺少完整性摘要")
        actual_integrity = hashlib.sha256(_canonical_json(payload)).hexdigest()
        if supplied_integrity != actual_integrity:
            raise AppError("E_SESSION_MISMATCH", "会话完整性摘要不匹配")
        version = payload.get("schema_version")
        if version != SESSION_SCHEMA_VERSION:
            raise AppError(
                "E_SESSION_MISMATCH",
                "会话版本不匹配",
                expected=SESSION_SCHEMA_VERSION,
                actual=version,
            )
        if payload.get("session_format") != "voice-music-repair":
            raise AppError("E_SESSION_MISMATCH", "会话格式标识不匹配")

        try:
            paths = payload["paths"]
            audio = payload["audio"]
            sample_rate = int(audio["sample_rate"])
            channels = int(audio["channels"])
            frames = int(audio["frames"])
            block_size = int(payload["block_size"])
            input_file_sha = str(audio["input_file_sha256"])
            output_path = _resolved_path(str(paths["output"]), self.path)
            input_path = _resolved_path(str(paths["input"]), self.path)
            cache_dir = _resolved_path(str(paths["block_cache"]), self.path)
            chain = ChainConfig.from_dict(payload["processing_chain"])
            intervals = [
                Interval.from_dict(item, total_frames=frames)
                for item in payload.get("intervals", [])
            ]
        except KeyError as exc:
            raise AppError("E_SESSION_MISMATCH", f"会话缺少字段: {exc}") from exc
        except (TypeError, ValueError) as exc:
            raise AppError("E_SESSION_MISMATCH", "会话字段类型非法", reason=str(exc)) from exc
        validate_sample_rate(sample_rate)
        if channels not in (1, 2):
            raise AppError("E_SESSION_MISMATCH", "会话声道数非法")
        if frames <= 0:
            raise AppError("E_SESSION_MISMATCH", "会话音频帧数非法")
        if not 128 <= block_size <= 1_048_576:
            raise AppError("E_SESSION_MISMATCH", "会话 block_size 非法")
        interval_list = validate_intervals(intervals, frames)

        info = WavInfo(
            sample_rate=sample_rate,
            channels=channels,
            frames=frames,
            sample_width=2,
        )
        if not input_path.is_file():
            raise AppError(
                "E_SESSION_MISMATCH", "会话引用的输入 WAV 不存在", path=str(input_path)
            )
        if verify_input and sha256_file(input_path) != input_file_sha:
            raise AppError("E_SESSION_MISMATCH", "输入 WAV 内容摘要不匹配")
        try:
            input_samples, actual_info = read_wav(input_path)
        except AppError as exc:
            raise AppError("E_SESSION_MISMATCH", "输入 WAV 无法读取", reason=exc.message) from exc
        if actual_info != info:
            raise AppError("E_SESSION_MISMATCH", "输入 WAV 参数与会话不一致")

        validate_output_file(output_path, info)
        if not cache_dir.is_dir():
            raise AppError("E_RECOVERY_INVALID", "块缓存目录缺失", cache=str(cache_dir))

        snapshot = SessionSnapshot(
            path=self.path,
            raw=payload,
            info=info,
            input_path=input_path,
            output_path=output_path,
            cache_dir=cache_dir,
            input_samples=input_samples,
            block_size=block_size,
            chain=chain,
            intervals=interval_list,
        )
        self._validate_completed_blocks(snapshot)
        if snapshot.complete:
            if len(snapshot.completed) != snapshot.block_count:
                raise AppError("E_SESSION_MISMATCH", "会话标记完成但块摘要不完整")
            expected_output_sha = payload.get("output_sha256")
            if not isinstance(expected_output_sha, str):
                raise AppError("E_SESSION_MISMATCH", "完成会话缺少输出 WAV 摘要")
            if sha256_file(output_path) != expected_output_sha:
                raise AppError("E_SESSION_MISMATCH", "输出 WAV 内容摘要不匹配")
        return snapshot

    def _validate_completed_blocks(self, snapshot: SessionSnapshot) -> None:
        seen: set[int] = set()
        for key, summary in snapshot.completed.items():
            if not isinstance(summary, dict):
                raise AppError("E_SESSION_MISMATCH", "块摘要必须是对象", block=key)
            try:
                index = int(summary["index"])
                start = int(summary["start_frame"])
                frames = int(summary["frames"])
                source_sha = str(summary["input_sha256"])
                cfg_sha = str(summary["config_sha256"])
                output_sha = str(summary["output_sha256"])
            except (KeyError, TypeError, ValueError) as exc:
                raise AppError("E_SESSION_MISMATCH", "块摘要字段非法", block=key) from exc
            if str(key) != str(index) or index in seen:
                raise AppError("E_SESSION_MISMATCH", "块索引重复或不连续", block=key)
            seen.add(index)
            if index < 0 or index >= snapshot.block_count:
                raise AppError("E_SESSION_MISMATCH", "块索引越界", block=index)
            expected_start, expected_frames, _ = snapshot.block_bounds(index)
            if start != expected_start or frames != expected_frames:
                raise AppError("E_SESSION_MISMATCH", "块边界摘要不匹配", block=index)
            expected_source, expected_config = snapshot.expected_summary(index)
            if source_sha != expected_source:
                raise AppError("E_SESSION_MISMATCH", "块输入摘要不匹配", block=index)
            if cfg_sha != expected_config:
                raise AppError("E_SESSION_MISMATCH", "块配置摘要不匹配", block=index)
            payload, origin = self.read_completed_block(snapshot, index)
            actual_sha = sha256_bytes(payload)
            if actual_sha != output_sha:
                raise AppError(
                    "E_SESSION_MISMATCH",
                    "块输出摘要不匹配",
                    block=index,
                    origin=origin,
                )

    def read_completed_block(
        self, snapshot: SessionSnapshot, block_index: int
    ) -> tuple[bytes, str]:
        summary = snapshot.completed.get(str(block_index))
        if summary is None:
            raise AppError("E_RECOVERY_INVALID", "块尚未完成", block=block_index)
        cache_path = snapshot.cache_path(block_index)
        if cache_path.is_file():
            try:
                payload = cache_path.read_bytes()
            except OSError as exc:
                raise AppError("E_RECOVERY_INVALID", "块缓存不可读", block=block_index) from exc
            if sha256_bytes(payload) == summary["output_sha256"]:
                return payload, "cache"
            raise AppError("E_SESSION_MISMATCH", "块缓存摘要不匹配", block=block_index)
        payload = read_output_block(
            snapshot.output_path,
            snapshot.info,
            block_index,
            snapshot.block_size,
        )
        if sha256_bytes(payload) != summary["output_sha256"]:
            raise AppError(
                "E_RECOVERY_INVALID",
                "已完成块同时缺失于缓存和输出",
                block=block_index,
            )
        atomic_write_bytes(cache_path, payload)
        return payload, "output"

    def write_completed_block(
        self,
        snapshot: SessionSnapshot,
        block_index: int,
        payload: bytes,
    ) -> str:
        expected_bytes = snapshot.block_bounds(block_index)[1] * snapshot.info.channels * 2
        if len(payload) != expected_bytes:
            raise AppError("E_INTERNAL", "块输出字节数非法", block=block_index)
        atomic_write_bytes(snapshot.cache_path(block_index), payload)
        return sha256_bytes(payload)

    def materialize_output(self, snapshot: SessionSnapshot) -> str:
        validate_output_file(snapshot.output_path, snapshot.info)
        temporary_fd, temporary_name = tempfile.mkstemp(
            prefix=f".{snapshot.output_path.name}.",
            suffix=".tmp",
            dir=str(snapshot.output_path.parent),
        )
        digest = hashlib.sha256()
        try:
            with os.fdopen(temporary_fd, "wb") as handle:
                header = wav_header(
                    snapshot.info.frames,
                    snapshot.info.sample_rate,
                    snapshot.info.channels,
                )
                handle.write(header)
                digest.update(header)
                for block_index in range(snapshot.block_count):
                    if str(block_index) in snapshot.completed:
                        payload, _ = self.read_completed_block(snapshot, block_index)
                    else:
                        frames = snapshot.block_bounds(block_index)[1]
                        payload = b"\0" * (frames * snapshot.info.channels * 2)
                    handle.write(payload)
                    digest.update(payload)
                handle.flush()
                os.fsync(handle.fileno())
            if os.path.getsize(temporary_name) != snapshot.info.file_size:
                raise AppError("E_INTERNAL", "生成的输出 WAV 大小非法")
            os.replace(temporary_name, snapshot.output_path)
            if os.name != "nt":
                try:
                    directory_fd = os.open(snapshot.output_path.parent, os.O_RDONLY)
                    try:
                        os.fsync(directory_fd)
                    finally:
                        os.close(directory_fd)
                except OSError:
                    pass
        except BaseException:
            try:
                os.unlink(temporary_name)
            except FileNotFoundError:
                pass
            raise
        return digest.hexdigest()

    def commit_summary(
        self,
        snapshot: SessionSnapshot,
        block_index: int,
        source_sha: str,
        cfg_sha: str,
        output_payload: bytes,
        output_sha: str,
    ) -> None:
        start, frames, _ = snapshot.block_bounds(block_index)
        snapshot.raw.setdefault("completed_blocks", {})[str(block_index)] = {
            "index": block_index,
            "start_frame": start,
            "frames": frames,
            "frames_per_channel": frames,
            "channels": snapshot.info.channels,
            "input_sha256": source_sha,
            "config_sha256": cfg_sha,
            "output_sha256": output_sha,
            "output_bytes": len(output_payload),
            "status": "committed",
        }
        snapshot.raw["complete"] = False
        snapshot.raw["output_sha256"] = None
        self.save(snapshot.raw)

    def remove_summary(self, snapshot: SessionSnapshot, block_index: int) -> None:
        snapshot.raw.setdefault("completed_blocks", {}).pop(str(block_index), None)

    def refresh_dirty(self, snapshot: SessionSnapshot) -> list[int]:
        dirty: list[int] = []
        completed = snapshot.raw.setdefault("completed_blocks", {})
        for key in list(completed.keys()):
            index = int(key)
            expected_source, expected_config = snapshot.expected_summary(index)
            summary = completed[key]
            if (
                summary.get("input_sha256") != expected_source
                or summary.get("config_sha256") != expected_config
            ):
                dirty.append(index)
                completed.pop(key)
        if dirty:
            snapshot.raw["complete"] = False
            snapshot.raw["output_sha256"] = None
        return sorted(dirty)

    def finalize(
        self, snapshot: SessionSnapshot, *, mark_complete: bool
    ) -> tuple[str, bool]:
        output_sha = self.materialize_output(snapshot)
        all_complete = len(snapshot.completed) == snapshot.block_count
        completed_now = mark_complete and all_complete
        snapshot.raw["complete"] = completed_now
        snapshot.raw["output_sha256"] = output_sha if completed_now else None
        snapshot.raw["materialized_sha256"] = output_sha
        self.save(snapshot.raw)
        return output_sha, completed_now

    def status(self, snapshot: SessionSnapshot | None = None) -> dict[str, Any]:
        data = snapshot or self.load()
        completed = data.completed
        total = data.block_count
        missing = [index for index in range(total) if str(index) not in completed]
        return {
            "ok": True,
            "session": str(data.path),
            "input": str(data.input_path),
            "output": str(data.output_path),
            "sample_rate": data.info.sample_rate,
            "channels": data.info.channels,
            "frames": data.info.frames,
            "block_size": data.block_size,
            "total_blocks": total,
            "committed_blocks": len(completed),
            "missing_blocks": missing,
            "complete": data.complete,
            "output_sha256": data.raw.get("output_sha256"),
            "materialized_sha256": data.raw.get("materialized_sha256"),
        }

