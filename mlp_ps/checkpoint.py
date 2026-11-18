from __future__ import annotations

import argparse
import json
import os
import shutil
import threading
import time
import uuid
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, MutableMapping, Optional, Sequence, Tuple

import torch

from .config import Config
from .errors import (
    BusinessError,
    E_CKPT_CORRUPT,
    E_CKPT_MISSING,
    E_INTERNAL,
    exit_code_for,
)
from .layout import Layout, LayoutEntry, entries_from_spec, validate_layout_spec
from .model import build_optimizer, ordered_parameter_names, parameter_shapes
from .tensor_codec import FLOAT32_BYTES, bytes_to_tensor, tensor_to_bytes
from .util import (
    BandwidthLimitedWriter,
    atomic_write_text,
    canonical_json_bytes,
    monotonic_millis,
    pretty_json_text,
    sha256_bytes,
    sha256_file,
    utc_timestamp,
)


CHECKPOINT_FORMAT = "mlp-ps-ckpt-v1"
COMPLETE_MARKER = ".complete"


@dataclass(frozen=True)
class PackedSegment:
    kind: str
    param_name: str
    full_shape: Tuple[int, ...]
    start: int
    length: int
    offset: int
    nbytes: int
    sha256: str

    @property
    def shape(self) -> Tuple[int, ...]:
        return (self.length,) + self.full_shape[1:]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "kind": self.kind,
            "param_name": self.param_name,
            "full_shape": list(self.full_shape),
            "start": self.start,
            "length": self.length,
            "shape": list(self.shape),
            "offset": self.offset,
            "nbytes": self.nbytes,
            "sha256": self.sha256,
        }


@dataclass(frozen=True)
class PackedShardState:
    shard_id: int
    segments: Tuple[PackedSegment, ...]
    payload: bytes
    consumed_samples: int

    @property
    def sha256(self) -> str:
        return sha256_bytes(self.payload)


@dataclass(frozen=True)
class CheckpointSnapshot:
    global_step: int
    layout_id: str
    layout_spec: Dict[str, Any]
    model_config: Dict[str, Any]
    optimizer_config: Dict[str, Any]
    shards: Mapping[int, PackedShardState]
    consumed_samples: Mapping[int, int]


@dataclass(frozen=True)
class CheckpointInfo:
    path: Path
    manifest: Dict[str, Any]
    global_step: int
    shard_bytes: Mapping[int, bytes]
    total_data_bytes: int

    @property
    def consumed_samples(self) -> Dict[int, int]:
        return {int(key): int(value) for key, value in self.manifest["consumed_samples"].items()}

    @property
    def layout_id(self) -> str:
        return str(self.manifest["layout_id"])


def _momentum_state(optimizer: torch.optim.Optimizer, param: torch.Tensor) -> Optional[torch.Tensor]:
    state = optimizer.state.get(param)
    if not state:
        return None
    value = state.get("momentum_buffer")
    if value is None:
        return None
    return value.detach().cpu().contiguous().to(dtype=torch.float32)


def pack_shard_state(
    layout: Layout,
    model_state: Mapping[str, torch.Tensor],
    momentum_state: Mapping[str, torch.Tensor],
    shard_id: int,
    consumed_samples: int,
) -> PackedShardState:
    chunks: List[bytes] = []
    segments: List[PackedSegment] = []
    offset = 0
    for entry in layout.entries_for_shard(shard_id):
        param_value = model_state[entry.param_name][entry.start : entry.start + entry.length]
        momentum_source = momentum_state.get(entry.param_name)
        if momentum_source is None:
            momentum_value = torch.zeros(entry.slice_shape, dtype=torch.float32)
        else:
            momentum_value = momentum_source[entry.start : entry.start + entry.length]

        for kind, value in (("param", param_value), ("momentum", momentum_value)):
            if tuple(value.shape) != entry.slice_shape:
                raise BusinessError(
                    E_INTERNAL,
                    "internal state slice shape does not match the layout",
                    {
                        "kind": kind,
                        "parameter": entry.param_name,
                        "shard_id": shard_id,
                        "got": list(value.shape),
                        "expected": list(entry.slice_shape),
                    },
                )
            data = tensor_to_bytes(value)
            segments.append(
                PackedSegment(
                    kind=kind,
                    param_name=entry.param_name,
                    full_shape=entry.full_shape,
                    start=entry.start,
                    length=entry.length,
                    offset=offset,
                    nbytes=len(data),
                    sha256=sha256_bytes(data),
                )
            )
            chunks.append(data)
            offset += len(data)
    return PackedShardState(
        shard_id=shard_id,
        segments=tuple(segments),
        payload=b"".join(chunks),
        consumed_samples=int(consumed_samples),
    )


def capture_snapshot(
    config: Config,
    layout: Layout,
    model: torch.nn.Module,
    optimizer: torch.optim.Optimizer,
    consumed_samples: Mapping[int, int],
    global_step: int,
) -> CheckpointSnapshot:
    state = {name: param.detach().cpu().contiguous() for name, param in model.named_parameters()}
    momentum: Dict[str, torch.Tensor] = {}
    for name, param in model.named_parameters():
        value = _momentum_state(optimizer, param)
        if value is not None:
            momentum[name] = value
    shards = {
        shard_id: pack_shard_state(
            layout,
            state,
            momentum,
            shard_id,
            consumed_samples[shard_id],
        )
        for shard_id in range(4)
    }
    return CheckpointSnapshot(
        global_step=int(global_step),
        layout_id=layout.layout_id,
        layout_spec=layout.spec,
        model_config={
            "seed": config.seed,
            "input_dim": config.input_dim,
            "hidden_dim": config.hidden_dim,
            "output_dim": config.output_dim,
        },
        optimizer_config={
            "name": "sgd",
            "learning_rate": config.learning_rate,
            "momentum": config.momentum,
            "dampening": 0.0,
            "weight_decay": 0.0,
            "nesterov": False,
        },
        shards=shards,
        consumed_samples={int(k): int(v) for k, v in consumed_samples.items()},
    )


def _write_limited(path: Path, data: bytes, bytes_per_second: float) -> None:
    with path.open("wb") as handle:
        writer = BandwidthLimitedWriter(handle, bytes_per_second)
        try:
            writer.write(data)
        finally:
            writer.close()


def _padding_bytes(size: int, step: int) -> bytes:
    if size <= 0:
        return b""
    pattern = f"mlp-ps-slow-disk-checkpoint-{step:08d}\n".encode("ascii")
    repeats = (size + len(pattern) - 1) // len(pattern)
    return (pattern * repeats)[:size]


def write_checkpoint(
    snapshot: CheckpointSnapshot,
    checkpoint_root: Path,
    bytes_per_second: float,
    padding_bytes: int,
) -> Dict[str, Any]:
    started = time.perf_counter()
    checkpoint_root.mkdir(parents=True, exist_ok=True)
    final_path = checkpoint_root / f"ckpt-{snapshot.global_step:08d}"
    if final_path.exists():
        raise BusinessError(
            E_INTERNAL,
            "checkpoint directory already exists",
            {"path": str(final_path)},
        )
    temporary_path = checkpoint_root / (
        f".tmp-{snapshot.global_step:08d}-{os.getpid()}-{uuid.uuid4().hex[:8]}"
    )
    temporary_path.mkdir(parents=True)

    total_bytes = 0
    shard_manifest: List[Dict[str, Any]] = []
    try:
        for shard_id in range(4):
            packed = snapshot.shards[shard_id]
            filename = f"shard-{shard_id}.bin"
            file_path = temporary_path / filename
            _write_limited(file_path, packed.payload, bytes_per_second)
            total_bytes += len(packed.payload)
            shard_manifest.append(
                {
                    "shard_id": shard_id,
                    "file": filename,
                    "file_bytes": len(packed.payload),
                    "sha256": packed.sha256,
                    "consumed_samples": packed.consumed_samples,
                    "segments": [segment.to_dict() for segment in packed.segments],
                }
            )

        padding = _padding_bytes(padding_bytes, snapshot.global_step)
        padding_manifest: Dict[str, Any] = {"file": None, "bytes": 0, "sha256": None}
        if padding:
            padding_file = temporary_path / "padding.bin"
            _write_limited(padding_file, padding, bytes_per_second)
            total_bytes += len(padding)
            padding_manifest = {
                "file": "padding.bin",
                "bytes": len(padding),
                "sha256": sha256_bytes(padding),
            }

        manifest: Dict[str, Any] = {
            "format": CHECKPOINT_FORMAT,
            "created_at_utc": utc_timestamp(),
            "global_step": snapshot.global_step,
            "layout_id": snapshot.layout_id,
            "layout": snapshot.layout_spec,
            "model": snapshot.model_config,
            "optimizer": snapshot.optimizer_config,
            "consumed_samples": {
                str(shard_id): int(snapshot.consumed_samples[shard_id])
                for shard_id in range(4)
            },
            "shards": shard_manifest,
            "padding": padding_manifest,
            "data_bytes": total_bytes,
        }
        manifest_bytes = pretty_json_text(manifest).encode("utf-8")
        _write_limited(temporary_path / "manifest.json", manifest_bytes, bytes_per_second)
        total_bytes += len(manifest_bytes)

        complete = {
            "format": CHECKPOINT_FORMAT,
            "global_step": snapshot.global_step,
            "manifest_sha256": sha256_bytes(manifest_bytes),
            "data_bytes": total_bytes,
        }
        complete_bytes = (pretty_json_text(complete) + "\n").encode("utf-8")
        with (temporary_path / COMPLETE_MARKER).open("wb") as handle:
            handle.write(complete_bytes)
            handle.flush()
            os.fsync(handle.fileno())

        os.replace(temporary_path, final_path)
    except Exception:
        shutil.rmtree(temporary_path, ignore_errors=True)
        raise

    elapsed = time.perf_counter() - started
    expected_elapsed = total_bytes / bytes_per_second if bytes_per_second > 0 else 0.0
    return {
        "status": "OK",
        "timestamp_utc": utc_timestamp(),
        "step": snapshot.global_step,
        "path": str(final_path),
        "bytes": total_bytes,
        "duration_ms": round(elapsed * 1000.0, 6),
        "expected_duration_ms_at_limit": round(expected_elapsed * 1000.0, 6),
        "target_bytes_per_second": bytes_per_second,
        "effective_bytes_per_second": round(total_bytes / elapsed, 3) if elapsed > 0 else 0.0,
        "within_bandwidth_limit": elapsed + 0.001 >= expected_elapsed,
    }


def _parse_manifest(manifest_bytes: bytes) -> Dict[str, Any]:
    try:
        manifest = json.loads(manifest_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint manifest is not valid JSON") from exc
    if not isinstance(manifest, dict) or manifest.get("format") != CHECKPOINT_FORMAT:
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint manifest has an unsupported format")
    return manifest


def _validate_segments(
    shard: Mapping[str, Any],
    layout_spec: Mapping[str, Any],
) -> None:
    shard_id = int(shard["shard_id"])
    expected_entries = [entry for entry in entries_from_spec(layout_spec) if entry.shard_id == shard_id]
    expected_keys = {
        (kind, entry.param_name, entry.start, entry.length)
        for entry in expected_entries
        for kind in ("param", "momentum")
    }
    seen_keys = set()
    previous_end = 0
    segments = shard.get("segments")
    if not isinstance(segments, list):
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint shard has no segment list", {"shard_id": shard_id})
    for segment in segments:
        try:
            kind = str(segment["kind"])
            name = str(segment["param_name"])
            start = int(segment["start"])
            length = int(segment["length"])
            full_shape = tuple(int(v) for v in segment["full_shape"])
            shape = tuple(int(v) for v in segment["shape"])
            offset = int(segment["offset"])
            nbytes = int(segment["nbytes"])
            digest = str(segment["sha256"])
        except (KeyError, TypeError, ValueError) as exc:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint segment metadata is invalid") from exc
        key = (kind, name, start, length)
        if key in seen_keys:
            raise BusinessError(E_CKPT_CORRUPT, "duplicate checkpoint segment", {"shard_id": shard_id})
        seen_keys.add(key)
        expected_shape = (length,) + full_shape[1:]
        expected_bytes = FLOAT32_BYTES
        for dimension in expected_shape:
            expected_bytes *= dimension
        if (
            offset != previous_end
            or nbytes != expected_bytes
            or shape != expected_shape
            or len(digest) != 64
        ):
            raise BusinessError(
                E_CKPT_CORRUPT,
                "checkpoint segment layout is invalid",
                {"shard_id": shard_id, "parameter": name, "kind": kind},
            )
        previous_end = offset + nbytes
    if seen_keys != expected_keys:
        raise BusinessError(
            E_CKPT_CORRUPT,
            "checkpoint segments do not exactly cover the parameter layout",
            {"shard_id": shard_id},
        )


def validate_checkpoint(path: str | Path) -> CheckpointInfo:
    checkpoint_path = Path(path)
    if not checkpoint_path.exists():
        raise BusinessError(E_CKPT_MISSING, "checkpoint path does not exist", {"path": str(checkpoint_path)})
    if not checkpoint_path.is_dir():
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint path is not a directory", {"path": str(checkpoint_path)})
    marker_path = checkpoint_path / COMPLETE_MARKER
    manifest_path = checkpoint_path / "manifest.json"
    if not marker_path.exists() or not manifest_path.exists():
        raise BusinessError(
            E_CKPT_CORRUPT,
            "checkpoint is incomplete",
            {"path": str(checkpoint_path)},
        )
    try:
        marker = json.loads(marker_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint completion marker is invalid") from exc
    manifest_bytes = manifest_path.read_bytes()
    if marker.get("manifest_sha256") != sha256_bytes(manifest_bytes):
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint manifest hash mismatch")
    manifest = _parse_manifest(manifest_bytes)

    try:
        layout_spec = manifest["layout"]
        validate_layout_spec(layout_spec)
        if str(manifest["layout_id"]) != sha256_bytes(canonical_json_bytes(layout_spec))[:16]:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint layout id does not match its layout")
        global_step = int(manifest["global_step"])
        if global_step < 0:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint global step is negative")
        consumed = manifest["consumed_samples"]
        if set(consumed.keys()) != {str(i) for i in range(4)}:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint consumed-sample map is incomplete")
        for value in consumed.values():
            if int(value) < 0:
                raise BusinessError(E_CKPT_CORRUPT, "checkpoint consumed-sample count is negative")
        shards = manifest["shards"]
        if len(shards) != 4:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint must contain exactly four shards")
    except BusinessError:
        raise
    except (KeyError, TypeError, ValueError) as exc:
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint manifest fields are invalid") from exc

    shard_bytes: Dict[int, bytes] = {}
    total_data_bytes = 0
    seen_shards = set()
    for shard in shards:
        try:
            shard_id = int(shard["shard_id"])
            filename = str(shard["file"])
            declared_bytes = int(shard["file_bytes"])
            declared_sha = str(shard["sha256"])
            consumed_samples = int(shard["consumed_samples"])
        except (KeyError, TypeError, ValueError) as exc:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint shard metadata is invalid") from exc
        if shard_id in seen_shards or not (0 <= shard_id < 4):
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint shard ids are invalid")
        seen_shards.add(shard_id)
        if int(consumed[str(shard_id)]) != consumed_samples:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint shard sample counters disagree")
        if Path(filename).name != filename:
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint shard file name is unsafe")
        file_path = checkpoint_path / filename
        if not file_path.exists():
            raise BusinessError(
                E_CKPT_CORRUPT,
                "checkpoint shard file is missing",
                {"shard_id": shard_id, "file": filename},
            )
        data = file_path.read_bytes()
        if len(data) != declared_bytes or sha256_bytes(data) != declared_sha:
            raise BusinessError(
                E_CKPT_CORRUPT,
                "checkpoint shard file hash or size mismatch",
                {"shard_id": shard_id},
            )
        _validate_segments(shard, layout_spec)
        segments = shard["segments"]
        expected_file_bytes = int(segments[-1]["offset"]) + int(segments[-1]["nbytes"])
        if expected_file_bytes != len(data):
            raise BusinessError(
                E_CKPT_CORRUPT,
                "checkpoint shard file contains uncovered bytes",
                {"shard_id": shard_id},
            )
        for segment in segments:
            offset = int(segment["offset"])
            nbytes = int(segment["nbytes"])
            if sha256_bytes(data[offset : offset + nbytes]) != segment["sha256"]:
                raise BusinessError(
                    E_CKPT_CORRUPT,
                    "checkpoint segment hash mismatch",
                    {"shard_id": shard_id, "parameter": segment["param_name"], "kind": segment["kind"]},
                )
        shard_bytes[shard_id] = data
        total_data_bytes += len(data)

    padding = manifest.get("padding", {})
    padding_file = padding.get("file")
    if padding_file:
        if Path(str(padding_file)).name != str(padding_file):
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint padding file name is unsafe")
        padding_path = checkpoint_path / str(padding_file)
        if not padding_path.exists():
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint padding file is missing")
        padding_data = padding_path.read_bytes()
        if len(padding_data) != int(padding.get("bytes", -1)) or sha256_bytes(padding_data) != padding.get(
            "sha256"
        ):
            raise BusinessError(E_CKPT_CORRUPT, "checkpoint padding file is corrupt")
        total_data_bytes += len(padding_data)

    if seen_shards != set(range(4)):
        raise BusinessError(E_CKPT_CORRUPT, "checkpoint does not contain all four shards")
    return CheckpointInfo(
        path=checkpoint_path,
        manifest=manifest,
        global_step=global_step,
        shard_bytes=shard_bytes,
        total_data_bytes=total_data_bytes,
    )


def load_checkpoint_shard(
    info: CheckpointInfo,
    shard_id: int,
) -> Tuple[Dict[str, torch.Tensor], Dict[str, torch.Tensor], int]:
    shard_manifest = next(
        shard for shard in info.manifest["shards"] if int(shard["shard_id"]) == shard_id
    )
    data = info.shard_bytes[shard_id]
    params: Dict[str, torch.Tensor] = {}
    momentum: Dict[str, torch.Tensor] = {}
    for segment in shard_manifest["segments"]:
        offset = int(segment["offset"])
        nbytes = int(segment["nbytes"])
        value = bytes_to_tensor(
            data[offset : offset + nbytes],
            [int(v) for v in segment["shape"]],
        )
        target = params if segment["kind"] == "param" else momentum
        target[str(segment["param_name"])] = value
    return params, momentum, int(shard_manifest["consumed_samples"])


def list_complete_checkpoints(root: str | Path) -> List[Path]:
    checkpoint_root = Path(root)
    if not checkpoint_root.exists():
        return []
    result: List[Path] = []
    for entry in checkpoint_root.iterdir():
        if entry.is_dir() and entry.name.startswith("ckpt-") and (entry / COMPLETE_MARKER).exists():
            result.append(entry)
    return sorted(result, key=lambda path: path.name)


def find_latest_checkpoint(root: str | Path, validate: bool = True) -> CheckpointInfo:
    candidates = list_complete_checkpoints(root)
    if not candidates:
        raise BusinessError(E_CKPT_MISSING, "no complete checkpoint is available", {"root": str(root)})
    path = candidates[-1]
    if validate:
        return validate_checkpoint(path)
    manifest = json.loads((path / "manifest.json").read_text(encoding="utf-8"))
    return CheckpointInfo(
        path=path,
        manifest=manifest,
        global_step=int(manifest["global_step"]),
        shard_bytes={},
        total_data_bytes=int(manifest.get("data_bytes", 0)),
    )


class CheckpointManager:
    def __init__(
        self,
        checkpoint_root: Path,
        bytes_per_second: float,
        padding_bytes: int,
        max_pending: int,
        events_path: Path,
    ) -> None:
        self.checkpoint_root = checkpoint_root
        self.bytes_per_second = bytes_per_second
        self.padding_bytes = padding_bytes
        self.max_pending = max(1, max_pending)
        self.events_path = events_path
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="checkpoint-writer")
        self._futures: List[Future] = []
        self._lock = threading.Lock()
        self._events_lock = threading.Lock()
        self._first_error: Optional[BaseException] = None

    def _record_event(self, event: Dict[str, Any]) -> None:
        with self._events_lock:
            self.events_path.parent.mkdir(parents=True, exist_ok=True)
            with self.events_path.open("a", encoding="utf-8", newline="\n") as handle:
                handle.write(json.dumps(event, sort_keys=True, separators=(",", ":")) + "\n")
                handle.flush()

    def _run(self, snapshot: CheckpointSnapshot) -> Dict[str, Any]:
        try:
            event = write_checkpoint(
                snapshot,
                self.checkpoint_root,
                self.bytes_per_second,
                self.padding_bytes,
            )
            event["status"] = "OK"
            self._record_event(event)
            return event
        except BaseException as exc:
            self._record_event(
                {
                    "timestamp_utc": utc_timestamp(),
                    "step": snapshot.global_step,
                    "status": "ERROR",
                    "error": repr(exc),
                }
            )
            with self._lock:
                if self._first_error is None:
                    self._first_error = exc
            raise

    def submit(self, snapshot: CheckpointSnapshot) -> Future:
        with self._lock:
            self._futures = [future for future in self._futures if not future.done()]
            if len(self._futures) >= self.max_pending:
                raise BusinessError(
                    E_INTERNAL,
                    "too many checkpoint writes are queued",
                    {"pending": len(self._futures), "max_pending": self.max_pending},
                )
            future = self._executor.submit(self._run, snapshot)
            self._futures.append(future)
            return future

    def write_sync(self, snapshot: CheckpointSnapshot) -> Dict[str, Any]:
        return self._run(snapshot)

    def pending_count(self) -> int:
        with self._lock:
            self._futures = [future for future in self._futures if not future.done()]
            return len(self._futures)

    def first_error(self) -> Optional[BaseException]:
        with self._lock:
            return self._first_error

    def flush(self, timeout_s: float = 60.0) -> None:
        with self._lock:
            futures = list(self._futures)
        deadline = time.perf_counter() + timeout_s
        for future in futures:
            remaining = max(0.0, deadline - time.perf_counter())
            future.result(timeout=remaining)
        with self._lock:
            self._futures = [future for future in self._futures if not future.done()]

    def close(self) -> None:
        self._executor.shutdown(wait=True, cancel_futures=False)

    def latest_complete(self, validate: bool = True) -> CheckpointInfo:
        return find_latest_checkpoint(self.checkpoint_root, validate=validate)


def _cmd_verify(args: argparse.Namespace) -> int:
    try:
        info = validate_checkpoint(args.path)
        print(
            pretty_json_text(
                {
                    "status": "OK",
                    "path": str(info.path),
                    "global_step": info.global_step,
                    "layout_id": info.layout_id,
                    "consumed_samples": info.consumed_samples,
                    "data_bytes": info.total_data_bytes,
                }
            ),
            end="",
        )
        return 0
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), end="")
        return exc.exit_code


def _cmd_latest(args: argparse.Namespace) -> int:
    try:
        info = find_latest_checkpoint(args.root, validate=not args.no_validate)
        print(
            pretty_json_text(
                {
                    "status": "OK",
                    "path": str(info.path),
                    "global_step": info.global_step,
                    "layout_id": info.layout_id,
                }
            ),
            end="",
        )
        return 0
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), end="")
        return exc.exit_code


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Checkpoint verification utility")
    subparsers = parser.add_subparsers(dest="command", required=True)
    verify = subparsers.add_parser("verify", help="verify one checkpoint directory")
    verify.add_argument("--path", required=True)
    verify.set_defaults(func=_cmd_verify)
    latest = subparsers.add_parser("latest", help="validate and print the latest checkpoint")
    latest.add_argument("--root", required=True)
    latest.add_argument("--no-validate", action="store_true")
    latest.set_defaults(func=_cmd_latest)
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
