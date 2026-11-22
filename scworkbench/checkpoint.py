from __future__ import annotations

import hashlib
import json
import os
import threading
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .profiles import PROTOCOL_VERSION

CHECKPOINT_FORMAT = "scwb-checkpoint"
CHECKPOINT_FORMAT_VERSION = 1


@dataclass(frozen=True)
class CheckpointLoad:
    status: str
    payload: dict[str, Any] | None = None
    reason: str = ""
    path: str = ""


class CheckpointManager:
    def __init__(
        self,
        directory: str | Path,
        session_id: str,
        content_hash: str,
        capabilities_hash: str,
        protocol_version: str = PROTOCOL_VERSION,
    ) -> None:
        self.directory = Path(directory)
        self.session_id = session_id
        self.content_hash = content_hash
        self.capabilities_hash = capabilities_hash
        self.protocol_version = protocol_version
        self.path = self.directory / "checkpoint.json"
        self.invalid_path = self.directory / "checkpoint.invalid.json"
        self._lock = threading.RLock()
        self._save_counter = 0
        self.directory.mkdir(parents=True, exist_ok=True)

    def _envelope(self, payload: dict[str, Any]) -> dict[str, Any]:
        canonical_payload = json.dumps(
            payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")
        ).encode("utf-8")
        return {
            "format": CHECKPOINT_FORMAT,
            "format_version": CHECKPOINT_FORMAT_VERSION,
            "protocol_version": self.protocol_version,
            "content_hash": self.content_hash,
            "capabilities_hash": self.capabilities_hash,
            "session_id": self.session_id,
            "payload_sha256": hashlib.sha256(canonical_payload).hexdigest(),
            "payload": payload,
        }

    def save(self, payload: dict[str, Any]) -> Path:
        with self._lock:
            self._save_counter += 1
            envelope = self._envelope(payload)
            data = (
                json.dumps(
                    envelope,
                    ensure_ascii=False,
                    sort_keys=True,
                    separators=(",", ":"),
                )
                + "\n"
            ).encode("utf-8")
            temp = self.directory / (
                f".checkpoint.tmp.{os.getpid()}.{threading.get_ident()}."
                f"{uuid.uuid4().hex}"
            )
            try:
                with open(temp, "wb") as handle:
                    handle.write(data)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temp, self.path)
                try:
                    dir_fd = os.open(self.directory, os.O_RDONLY)
                except OSError:
                    dir_fd = -1
                if dir_fd >= 0:
                    try:
                        os.fsync(dir_fd)
                    finally:
                        os.close(dir_fd)
            finally:
                if temp.exists():
                    temp.unlink(missing_ok=True)
            return self.path

    def _invalidate(self, reason: str) -> CheckpointLoad:
        if self.path.exists():
            try:
                os.replace(self.path, self.invalid_path)
            except OSError:
                pass
        return CheckpointLoad(
            status="invalid", reason=reason, path=str(self.path)
        )

    def load(self) -> CheckpointLoad:
        with self._lock:
            if not self.path.exists():
                return CheckpointLoad(
                    status="missing",
                    reason="尚无检查点",
                    path=str(self.path),
                )
            try:
                envelope = json.loads(self.path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError) as exc:
                return self._invalidate(f"检查点不可读或 JSON 损坏: {exc}")
            if not isinstance(envelope, dict):
                return self._invalidate("检查点顶层不是对象")
            if envelope.get("format") != CHECKPOINT_FORMAT:
                return self._invalidate("检查点格式标识不匹配")
            if envelope.get("format_version") != CHECKPOINT_FORMAT_VERSION:
                return self._invalidate("检查点格式版本不匹配")
            if envelope.get("protocol_version") != self.protocol_version:
                return self._invalidate(
                    "协议版本不匹配: "
                    f"{envelope.get('protocol_version')} != {self.protocol_version}"
                )
            if envelope.get("content_hash") != self.content_hash:
                return self._invalidate("会话内容哈希不匹配")
            if envelope.get("capabilities_hash") != self.capabilities_hash:
                return self._invalidate("能力协商哈希不匹配")
            if envelope.get("session_id") != self.session_id:
                return self._invalidate("会话标识不匹配")
            payload = envelope.get("payload")
            if not isinstance(payload, dict):
                return self._invalidate("检查点 payload 缺失")
            canonical_payload = json.dumps(
                payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")
            ).encode("utf-8")
            actual_digest = hashlib.sha256(canonical_payload).hexdigest()
            if envelope.get("payload_sha256") != actual_digest:
                return self._invalidate("检查点 payload 摘要不匹配")
            return CheckpointLoad(
                status="loaded", payload=payload, path=str(self.path)
            )

    def clear(self) -> None:
        with self._lock:
            self.path.unlink(missing_ok=True)
            self.invalid_path.unlink(missing_ok=True)
