"""Atomic checkpoint chain and deterministic recovery."""

from __future__ import annotations

import json
import os
import re
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping

from .errors import E_IO, E_RECOVERY_INVALID, AppError
from .params import ParameterSet, canonical_json_bytes, sha256_bytes


CHECKPOINT_FORMAT = "inverse-imaging-checkpoint"
CHECKPOINT_VERSION = 1
_CHECKPOINT_PATTERN = re.compile(r"^checkpoint-(\d{8})\.json$")


@dataclass(frozen=True)
class PublishedSnapshot:
    handle: str
    step: int
    loss: float
    cumulative_loss: float
    params: dict[str, object]
    gradient_norms: dict[str, float]
    history: tuple[dict[str, object], ...]
    reduce_hash: str
    checkpoint_hash: str

    def to_dict(self) -> dict[str, object]:
        return {
            "handle": self.handle,
            "step": self.step,
            "loss": self.loss,
            "cumulative_loss": self.cumulative_loss,
            "params": self.params,
            "gradient_norms": self.gradient_norms,
            "history": list(self.history),
            "reduce_hash": self.reduce_hash,
            "checkpoint_hash": self.checkpoint_hash,
        }


@dataclass(frozen=True)
class CheckpointState:
    step: int
    params: ParameterSet
    cumulative_loss: float
    last_loss: float
    gradient_norms: dict[str, float]
    reduce_hash: str
    chunk_hashes: tuple[str, ...]
    graph_versions: dict[str, int]
    optimizer_state: dict[str, object]
    history: tuple[dict[str, object], ...]
    self_hash: str
    prev_hash: str
    path: Path

    @property
    def snapshot(self) -> PublishedSnapshot:
        handle = f"{self.step}:{self.self_hash}"
        return PublishedSnapshot(
            handle=handle,
            step=self.step,
            loss=self.last_loss,
            cumulative_loss=self.cumulative_loss,
            params=self.params.nested(),
            gradient_norms=dict(self.gradient_norms),
            history=self.history,
            reduce_hash=self.reduce_hash,
            checkpoint_hash=self.self_hash,
        )


class CheckpointManager:
    def __init__(self, directory: str | Path, scene_id: str) -> None:
        self.directory = Path(directory).resolve()
        self.checkpoint_dir = self.directory / "checkpoints"
        self.trajectory_path = self.directory / "trajectory.jsonl"
        self.scene_id = scene_id
        try:
            self.checkpoint_dir.mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            raise AppError(E_IO, f"cannot create checkpoint directory: {exc}", "checkpoint_dir") from exc

    def commit(
        self,
        *,
        step: int,
        params: ParameterSet,
        cumulative_loss: float,
        last_loss: float,
        gradient_norms: Mapping[str, float],
        reduce_hash: str,
        chunk_hashes: Iterable[str],
        optimizer_state: Mapping[str, object] | None = None,
        history: Iterable[Mapping[str, object]] = (),
    ) -> CheckpointState:
        if step < 0:
            raise ValueError("checkpoint step must be non-negative")
        previous = self.load_latest(strict=True)
        if previous is None:
            if step != 0:
                raise AppError(
                    E_RECOVERY_INVALID,
                    "first checkpoint must be step zero",
                    "step",
                    {"step": step},
                )
            prev_hash = ""
        else:
            if step != previous.step + 1:
                raise AppError(
                    E_RECOVERY_INVALID,
                    "checkpoint steps must be contiguous",
                    "step",
                    {"previous": previous.step, "step": step},
                )
            prev_hash = previous.self_hash
        history_list = [dict(item) for item in history]
        body: dict[str, Any] = {
            "format": CHECKPOINT_FORMAT,
            "format_version": CHECKPOINT_VERSION,
            "scene_id": self.scene_id,
            "step": step,
            "params": params.to_dict(),
            "cumulative_loss": float(cumulative_loss),
            "last_loss": float(last_loss),
            "gradient_norms": {
                str(key): float(value) for key, value in gradient_norms.items()
            },
            "reduce_hash": reduce_hash,
            "chunk_hashes": list(chunk_hashes),
            "graph_versions": {
                str(key): int(value) for key, value in params.versions.items()
            },
            "optimizer_state": _json_safe(optimizer_state or {}),
            "history": history_list,
            "prev_hash": prev_hash,
        }
        self_hash = sha256_bytes(canonical_json_bytes(body))
        payload = {**body, "self_hash": self_hash}
        target = self.checkpoint_dir / f"checkpoint-{step:08d}.json"
        encoded = json.dumps(
            payload,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
            allow_nan=False,
        ).encode("utf-8")
        self._atomic_write(target, encoded)
        self._write_trajectory(history_list)
        return CheckpointState(
            step=step,
            params=params.clone(),
            cumulative_loss=float(cumulative_loss),
            last_loss=float(last_loss),
            gradient_norms={str(k): float(v) for k, v in gradient_norms.items()},
            reduce_hash=reduce_hash,
            chunk_hashes=tuple(chunk_hashes),
            graph_versions=dict(params.versions),
            optimizer_state=_json_safe(optimizer_state or {}),
            history=tuple(history_list),
            self_hash=self_hash,
            prev_hash=prev_hash,
            path=target,
        )

    def load_latest(self, *, strict: bool = True) -> CheckpointState | None:
        paths = sorted(
            path
            for path in self.checkpoint_dir.glob("checkpoint-*.json")
            if _CHECKPOINT_PATTERN.match(path.name)
        )
        if not paths:
            return None
        states: list[CheckpointState] = []
        previous: CheckpointState | None = None
        for expected_step, path in enumerate(paths):
            state = self._read_checkpoint(path, expected_step)
            if state.step != expected_step:
                raise AppError(
                    E_RECOVERY_INVALID,
                    "checkpoint sequence has a gap",
                    "step",
                    {"expected": expected_step, "actual": state.step, "path": str(path)},
                )
            expected_prev = previous.self_hash if previous is not None else ""
            if state.prev_hash != expected_prev:
                raise AppError(
                    E_RECOVERY_INVALID,
                    "checkpoint hash chain is broken",
                    "prev_hash",
                    {
                        "step": state.step,
                        "expected": expected_prev,
                        "actual": state.prev_hash,
                    },
                )
            states.append(state)
            previous = state
        latest = states[-1]
        if strict:
            self._write_trajectory(latest.history)
        return latest

    def _read_checkpoint(self, path: Path, expected_step: int) -> CheckpointState:
        try:
            raw = path.read_bytes()
            payload = json.loads(raw.decode("utf-8"))
        except (OSError, UnicodeDecodeError, ValueError) as exc:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint is unreadable",
                "checkpoint",
                {"path": str(path), "error": str(exc)},
            ) from exc
        if not isinstance(payload, dict):
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint root must be an object",
                "checkpoint",
                {"path": str(path)},
            )
        self_hash = payload.pop("self_hash", None)
        if not isinstance(self_hash, str):
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint self_hash is missing",
                "self_hash",
                {"path": str(path)},
            )
        actual_hash = sha256_bytes(canonical_json_bytes(payload))
        if actual_hash != self_hash:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint content hash mismatch",
                "self_hash",
                {
                    "path": str(path),
                    "expected": actual_hash,
                    "actual": self_hash,
                },
            )
        if payload.get("format") != CHECKPOINT_FORMAT:
            raise AppError(
                E_RECOVERY_INVALID,
                "unsupported checkpoint format",
                "format",
                {"path": str(path)},
            )
        if payload.get("format_version") != CHECKPOINT_VERSION:
            raise AppError(
                E_RECOVERY_INVALID,
                "unsupported checkpoint version",
                "format_version",
                {"path": str(path)},
            )
        if payload.get("scene_id") != self.scene_id:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint belongs to a different scene",
                "scene_id",
                {"path": str(path)},
            )
        step = payload.get("step")
        if not isinstance(step, int) or step != expected_step:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint step does not match its filename",
                "step",
                {"path": str(path), "expected": expected_step, "actual": step},
            )
        try:
            params = ParameterSet.from_dict(payload["params"])
        except (KeyError, TypeError, ValueError, AppError) as exc:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint parameters are invalid",
                "params",
                {"path": str(path), "error": str(exc)},
            ) from exc
        history = payload.get("history")
        if not isinstance(history, list) or not all(isinstance(item, dict) for item in history):
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint history is invalid",
                "history",
                {"path": str(path)},
            )
        if len(history) != step + 1:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint history length does not match the committed step",
                "history",
                {"path": str(path), "expected": step + 1, "actual": len(history)},
            )
        if history and int(history[-1].get("step", -1)) != step:
            raise AppError(
                E_RECOVERY_INVALID,
                "last trajectory event does not match the committed step",
                "history",
                {"path": str(path), "expected": step, "actual": history[-1].get("step")},
            )
        try:
            state = CheckpointState(
                step=step,
                params=params,
                cumulative_loss=float(payload["cumulative_loss"]),
                last_loss=float(payload["last_loss"]),
                gradient_norms={
                    str(key): float(value)
                    for key, value in payload["gradient_norms"].items()
                },
                reduce_hash=str(payload["reduce_hash"]),
                chunk_hashes=tuple(str(value) for value in payload["chunk_hashes"]),
                graph_versions={
                    str(key): int(value)
                    for key, value in payload["graph_versions"].items()
                },
                optimizer_state=dict(payload.get("optimizer_state", {})),
                history=tuple(history),
                self_hash=self_hash,
                prev_hash=str(payload["prev_hash"]),
                path=path,
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise AppError(
                E_RECOVERY_INVALID,
                "checkpoint fields are invalid",
                "checkpoint",
                {"path": str(path), "error": str(exc)},
            ) from exc
        if state.graph_versions != state.params.versions:
            raise AppError(
                E_RECOVERY_INVALID,
                "graph versions do not match parameter versions",
                "graph_versions",
                {"path": str(path)},
            )
        return state

    def _write_trajectory(self, history: list[dict[str, object]]) -> None:
        lines = [
            json.dumps(
                item,
                sort_keys=True,
                separators=(",", ":"),
                ensure_ascii=True,
                allow_nan=False,
            )
            for item in history
        ]
        payload = ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")
        self._atomic_write(self.trajectory_path, payload)

    @staticmethod
    def _atomic_write(target: Path, payload: bytes) -> None:
        target.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{target.name}.", suffix=".tmp", dir=str(target.parent)
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, target)
            try:
                directory_fd = os.open(target.parent, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
        except OSError as exc:
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                pass
            raise AppError(
                E_IO,
                f"atomic checkpoint write failed: {exc}",
                "checkpoint",
                {"path": str(target)},
            ) from exc


def _json_safe(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_safe(item) for item in value]
    if isinstance(value, bool) or value is None:
        return value
    if isinstance(value, (int, float)):
        number = float(value) if isinstance(value, float) else value
        if isinstance(number, float) and not (number == number and abs(number) != float("inf")):
            raise ValueError("non-finite checkpoint value")
        return number
    return str(value)

