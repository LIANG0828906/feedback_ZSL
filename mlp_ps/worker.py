from __future__ import annotations

import argparse
import hashlib
import json
import os
import socket
import sys
import time
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional, Sequence, Tuple

import torch

from .checkpoint import CheckpointInfo, load_checkpoint_shard, validate_checkpoint
from .config import Config, load_config
from .data import local_batch, train_dataset
from .determinism import configure_deterministic_runtime
from .errors import (
    BusinessError,
    E_CKPT_CORRUPT,
    E_CKPT_MISSING,
    E_LAYOUT_CONFLICT,
    E_NETWORK,
    E_PARAM_SHAPE,
    E_PROGRESS_RANGE,
    E_PROTOCOL,
    E_INTERNAL,
    exit_code_for,
)
from .layout import LayoutEntry, entries_from_spec, parameter_shapes_from_spec, validate_layout_spec
from .model import (
    build_model,
    full_gradient_snapshot,
    load_model_state,
    model_state_by_name,
    zero_gradients,
)
from .protocol import Frame, sync_read_frame, sync_send_frame
from .tensor_codec import pack_named_tensors, unpack_named_tensors
from .util import atomic_write_text, pretty_json_text, utc_timestamp


PROTOCOL_VERSION = 1


def _safe_worker_name(worker_id: str) -> str:
    return "".join(character if character.isalnum() or character in "-_." else "_" for character in worker_id)


def _write_worker_result(run_dir: Path, worker_id: str, result: Mapping[str, Any]) -> None:
    result_path = run_dir / f"worker_result_{_safe_worker_name(worker_id)}.json"
    atomic_write_text(result_path, pretty_json_text(result))


def _wait_for_trigger(path: Path, timeout_s: float) -> None:
    deadline = time.perf_counter() + timeout_s
    while not path.exists():
        if time.perf_counter() >= deadline:
            raise BusinessError(
                E_PROTOCOL,
                "standby worker did not receive the replacement trigger",
                {"trigger": str(path), "timeout_s": timeout_s},
            )
        time.sleep(0.01)


def _connect(config: Config, timeout_s: float = 15.0) -> socket.socket:
    deadline = time.perf_counter() + timeout_s
    last_error: Optional[BaseException] = None
    while time.perf_counter() < deadline:
        try:
            sock = socket.create_connection((config.host, config.port), timeout=5.0)
            sock.settimeout(config.step_timeout_s + 30.0)
            sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            return sock
        except OSError as exc:
            last_error = exc
            time.sleep(0.05)
    raise BusinessError(
        E_NETWORK,
        "worker could not connect to the parameter server",
        {
            "host": config.host,
            "port": config.port,
            "error": repr(last_error),
        },
    )


class TrainingWorker:
    def __init__(
        self,
        config: Config,
        worker_id: str,
        shard_id: int,
        role: str,
        resume_consumed_samples: Optional[int] = None,
        known_layout_id: Optional[str] = None,
        inject_bad_gradient_shape: bool = False,
    ) -> None:
        self.config = config
        self.worker_id = worker_id
        self.shard_id = shard_id
        self.role = role
        self.resume_consumed_samples = resume_consumed_samples
        self.known_layout_id = known_layout_id
        self.inject_bad_gradient_shape = inject_bad_gradient_shape
        configure_deterministic_runtime(config.seed)
        self.model = build_model(config.seed, config.input_dim, config.hidden_dim, config.output_dim)
        self.train_features, self.train_targets = train_dataset(config)
        self.sock: Optional[socket.socket] = None
        self.layout_spec: Optional[Dict[str, Any]] = None
        self.layout_id: Optional[str] = None
        self.entries: Tuple[LayoutEntry, ...] = ()
        self.parameter_order: List[str] = []
        self.parameter_shapes: Dict[str, Tuple[int, ...]] = {}
        self.owned_params: Dict[str, torch.Tensor] = {}
        self.recovered_momentum: Dict[str, torch.Tensor] = {}
        self.consumed_samples = 0
        self.last_step = 0
        self.checkpoint_info: Optional[CheckpointInfo] = None
        self.checkpoint_consumed_samples: Optional[int] = None
        self.wal_replayed_steps = 0

    def _send(self, header: Mapping[str, Any], payload: bytes = b"") -> None:
        if self.sock is None:
            raise ConnectionError("socket is not connected")
        sync_send_frame(self.sock, header, payload)

    def _receive(self) -> Frame:
        if self.sock is None:
            raise ConnectionError("socket is not connected")
        return sync_read_frame(self.sock, self.config.max_frame_bytes)

    def _send_client_error(self, error: BusinessError) -> None:
        try:
            self._send(
                {
                    "type": "CLIENT_ERROR",
                    "protocol": PROTOCOL_VERSION,
                    "worker_id": self.worker_id,
                    "shard_id": self.shard_id,
                    **error.to_dict(),
                }
            )
        except (ConnectionError, OSError):
            pass

    def _register(self) -> None:
        header = {
            "type": "REGISTER",
            "protocol": PROTOCOL_VERSION,
            "worker_id": self.worker_id,
            "shard_id": self.shard_id,
            "role": self.role,
            "known_layout_id": self.known_layout_id,
            "resume_consumed_samples": self.resume_consumed_samples,
        }
        self._send(header)
        frame = self._receive()
        if frame.message_type == "ABORT":
            raise BusinessError(
                str(frame.header.get("code", E_PROTOCOL)),
                str(frame.header.get("message", "server aborted registration")),
                frame.header.get("details", {}) if isinstance(frame.header.get("details"), dict) else {},
            )
        if frame.message_type != "REGISTERED":
            raise BusinessError(
                E_PROTOCOL,
                "parameter server did not acknowledge registration",
                {"type": frame.message_type},
            )
        if str(frame.header.get("layout_id")) != str(frame.header.get("layout", {}).get("layout_id")):
            raise BusinessError(E_PROTOCOL, "REGISTERED layout identifiers disagree")
        layout_spec = frame.header.get("layout", {}).get("spec")
        if not isinstance(layout_spec, dict):
            raise BusinessError(E_PROTOCOL, "REGISTERED response is missing the layout spec")
        validate_layout_spec(layout_spec)
        self.layout_spec = layout_spec
        self.layout_id = str(frame.header["layout_id"])
        self.entries = tuple(
            entry for entry in entries_from_spec(layout_spec) if entry.shard_id == self.shard_id
        )
        self.parameter_shapes = parameter_shapes_from_spec(layout_spec)
        self.parameter_order = list(self.parameter_shapes.keys())
        checkpoint = frame.header.get("checkpoint")
        if checkpoint is not None:
            checkpoint_path = Path(str(checkpoint["path"]))
            info = validate_checkpoint(checkpoint_path)
            if info.layout_id != self.layout_id:
                raise BusinessError(
                    E_CKPT_CORRUPT,
                    "checkpoint layout does not match the registered shard layout",
                    {
                        "checkpoint_layout_id": info.layout_id,
                        "layout_id": self.layout_id,
                    },
                )
            if checkpoint.get("manifest_sha256") is not None:
                manifest_path = info.path / "manifest.json"
                actual_sha = hashlib.sha256(manifest_path.read_bytes()).hexdigest()
                if actual_sha != str(checkpoint["manifest_sha256"]):
                    raise BusinessError(E_CKPT_CORRUPT, "checkpoint manifest hash changed during registration")
            self.checkpoint_info = info
            _, _, self.checkpoint_consumed_samples = load_checkpoint_shard(info, self.shard_id)
        else:
            self.checkpoint_info = None
            self.checkpoint_consumed_samples = None

    def _handle_start(self, frame: Frame) -> None:
        if self.layout_id is None or self.layout_spec is None:
            raise BusinessError(E_PROTOCOL, "layout metadata is unavailable at START")
        if str(frame.header.get("layout_id")) != self.layout_id:
            raise BusinessError(E_PROTOCOL, "START layout id mismatch")
        if int(frame.header.get("shard_id", -1)) != self.shard_id:
            raise BusinessError(E_PROTOCOL, "START shard id mismatch")
        if str(frame.header.get("worker_id")) != self.worker_id:
            raise BusinessError(E_PROTOCOL, "START worker id mismatch")
        order = frame.header.get("parameter_order")
        if not isinstance(order, list) or [str(name) for name in order] != self.parameter_order:
            raise BusinessError(E_PROTOCOL, "START parameter order mismatch")
        param_segments = frame.header.get("param_segments")
        momentum_segments = frame.header.get("momentum_segments")
        if not isinstance(param_segments, list) or not isinstance(momentum_segments, list):
            raise BusinessError(E_PARAM_SHAPE, "START tensor descriptors are missing")
        param_payload_bytes = sum(int(segment["nbytes"]) for segment in param_segments)
        if param_payload_bytes > len(frame.payload):
            raise BusinessError(E_PARAM_SHAPE, "START payload boundary is invalid")
        try:
            parameters = unpack_named_tensors(
                param_segments,
                frame.payload[:param_payload_bytes],
                self.parameter_order,
                self.parameter_shapes,
            )
            momentum_names = [entry.param_name for entry in self.entries]
            momentum_shapes = {entry.param_name: entry.slice_shape for entry in self.entries}
            momentum = unpack_named_tensors(
                momentum_segments,
                frame.payload[param_payload_bytes:],
                momentum_names,
                momentum_shapes,
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise BusinessError(
                E_PARAM_SHAPE,
                "START tensor shapes do not match the registered layout",
                {"error": str(exc)},
            ) from exc
        load_model_state(self.model, parameters)
        self.owned_params = {
            entry.param_name: parameters[entry.param_name][
                entry.start : entry.start + entry.length
            ].clone()
            for entry in self.entries
        }
        self.recovered_momentum = momentum
        self.consumed_samples = int(frame.header.get("consumed_samples", -1))
        self.last_step = int(frame.header.get("global_step", -1))
        self.wal_replayed_steps = int(frame.header.get("wal_replayed_steps", 0))
        if self.last_step < 0 or self.consumed_samples < 0:
            raise BusinessError(E_PROGRESS_RANGE, "START contains negative progress values")
        expected_consumed = self.last_step * self.config.local_batch_size
        if self.consumed_samples != expected_consumed:
            raise BusinessError(
                E_PROGRESS_RANGE,
                "START sample progress is inconsistent with the global step",
                {
                    "consumed_samples": self.consumed_samples,
                    "expected_consumed_samples": expected_consumed,
                    "global_step": self.last_step,
                },
            )
        if self.checkpoint_info is not None:
            if self.checkpoint_info.global_step > self.last_step:
                raise BusinessError(
                    E_CKPT_CORRUPT,
                    "checkpoint anchor is newer than the resumed training state",
                    {
                        "checkpoint_step": self.checkpoint_info.global_step,
                        "global_step": self.last_step,
                    },
                )
            if self.wal_replayed_steps != self.last_step - self.checkpoint_info.global_step:
                raise BusinessError(
                    E_PROGRESS_RANGE,
                    "recovery-log replay count does not match the checkpoint anchor",
                    {
                        "wal_replayed_steps": self.wal_replayed_steps,
                        "checkpoint_step": self.checkpoint_info.global_step,
                        "global_step": self.last_step,
                    },
                )
        self._send(
            {
                "type": "READY",
                "protocol": PROTOCOL_VERSION,
                "worker_id": self.worker_id,
                "shard_id": self.shard_id,
                "layout_id": self.layout_id,
                "step": self.last_step,
            }
        )

    def _handle_train(self, frame: Frame) -> None:
        step = int(frame.header.get("step", -1))
        if step != self.last_step + 1:
            raise BusinessError(
                E_PROTOCOL,
                "TRAIN step is not contiguous with local progress",
                {"local_step": self.last_step, "received_step": step},
            )
        if str(frame.header.get("layout_id")) != self.layout_id:
            raise BusinessError(E_PROTOCOL, "TRAIN layout id mismatch")
        if int(frame.header.get("shard_id", -1)) != self.shard_id:
            raise BusinessError(E_PROTOCOL, "TRAIN shard id mismatch")
        sample_start = int(frame.header.get("sample_start", -1))
        sample_end = int(frame.header.get("sample_end", -1))
        expected_start, expected_end = self._expected_sample_range(step)
        if (sample_start, sample_end) != (expected_start, expected_end):
            raise BusinessError(
                E_PROGRESS_RANGE,
                "TRAIN sample range does not match deterministic data partitioning",
                {
                    "received_start": sample_start,
                    "received_end": sample_end,
                    "expected_start": expected_start,
                    "expected_end": expected_end,
                },
            )
        param_segments = frame.header.get("param_segments")
        if not isinstance(param_segments, list):
            raise BusinessError(E_PARAM_SHAPE, "TRAIN parameter descriptors are missing")
        try:
            parameters = unpack_named_tensors(
                param_segments,
                frame.payload,
                self.parameter_order,
                self.parameter_shapes,
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise BusinessError(
                E_PARAM_SHAPE,
                "TRAIN parameter shape mismatch",
                {"error": str(exc)},
            ) from exc
        load_model_state(self.model, parameters)
        features, targets, _, _ = local_batch(
            self.train_features,
            self.train_targets,
            step - 1,
            self.shard_id,
            self.config,
        )
        zero_gradients(self.model)
        prediction = self.model(features)
        loss = torch.mean((prediction - targets) ** 2)
        loss.backward()
        gradients = full_gradient_snapshot(self.model)

        if self.inject_bad_gradient_shape:
            first_name = self.parameter_order[0]
            gradients[first_name] = gradients[first_name].transpose(0, 1).contiguous()

        gradient_segments, gradient_payload = pack_named_tensors(
            self.parameter_order, gradients
        )
        state = model_state_by_name(self.model)
        owned_names = [entry.param_name for entry in self.entries]
        owned_tensors = {
            entry.param_name: state[entry.param_name][
                entry.start : entry.start + entry.length
            ]
            for entry in self.entries
        }
        owned_segments, owned_payload = pack_named_tensors(owned_names, owned_tensors)
        payload = gradient_payload + owned_payload
        self.consumed_samples += self.config.local_batch_size
        response = {
            "type": "STEP_RESULT",
            "protocol": PROTOCOL_VERSION,
            "step": step,
            "layout_id": self.layout_id,
            "worker_id": self.worker_id,
            "shard_id": self.shard_id,
            "consumed_samples_after": self.consumed_samples,
            "gradient_segments": gradient_segments,
            "owned_param_segments": owned_segments,
            "gradient_payload_bytes": len(gradient_payload),
        }
        self._send(response, payload)
        self.owned_params = {name: value.clone() for name, value in owned_tensors.items()}
        self.last_step = step
        fault = self.config.fault
        if (
            self.role == "initial"
            and fault.enabled
            and self.shard_id == fault.shard_id
            and step >= fault.after_step
        ):
            raise _InjectedDisconnect(step)

    def _expected_sample_range(self, step: int) -> Tuple[int, int]:
        start = (step - 1) * self.config.batch_size + self.shard_id * self.config.local_batch_size
        return start, start + self.config.local_batch_size

    def run(self) -> int:
        result: Dict[str, Any] = {
            "worker_id": self.worker_id,
            "shard_id": self.shard_id,
            "role": self.role,
            "pid": os.getpid(),
        }
        try:
            self.sock = _connect(self.config)
            self._register()
            while True:
                frame = self._receive()
                if frame.message_type == "START":
                    self._handle_start(frame)
                elif frame.message_type == "TRAIN":
                    self._handle_train(frame)
                elif frame.message_type == "ABORT":
                    error = BusinessError(
                        str(frame.header.get("code", E_PROTOCOL)),
                        str(frame.header.get("message", "server aborted the job")),
                        frame.header.get("details", {})
                        if isinstance(frame.header.get("details"), dict)
                        else {},
                    )
                    result.update(
                        {
                            "status": "ABORTED",
                            "timestamp_utc": utc_timestamp(),
                            "last_step": self.last_step,
                            **error.to_dict(),
                        }
                    )
                    _write_worker_result(self.config.run_dir, self.worker_id, result)
                    return error.exit_code
                elif frame.message_type == "SHUTDOWN":
                    result.update(
                        {
                            "status": "OK",
                            "timestamp_utc": utc_timestamp(),
                            "last_step": self.last_step,
                            "consumed_samples": self.consumed_samples,
                            "checkpoint_step": self.checkpoint_info.global_step
                            if self.checkpoint_info is not None
                            else None,
                            "checkpoint_consumed_samples": self.checkpoint_consumed_samples,
                            "wal_replayed_steps": self.wal_replayed_steps,
                        }
                    )
                    _write_worker_result(self.config.run_dir, self.worker_id, result)
                    return 0
                else:
                    raise BusinessError(
                        E_PROTOCOL,
                        "worker received an unexpected server message",
                        {"type": frame.message_type},
                    )
        except _InjectedDisconnect as injected:
            result.update(
                {
                    "status": "DISCONNECTED",
                    "timestamp_utc": utc_timestamp(),
                    "last_step": injected.step,
                    "fault": "configured_connection_close",
                }
            )
            _write_worker_result(self.config.run_dir, self.worker_id, result)
            return 0
        except BusinessError as exc:
            result.update({"status": "ERROR", "timestamp_utc": utc_timestamp(), **exc.to_dict()})
            _write_worker_result(self.config.run_dir, self.worker_id, result)
            if exc.code in {E_PARAM_SHAPE, E_PROGRESS_RANGE, E_CKPT_CORRUPT, E_PROTOCOL}:
                self._send_client_error(exc)
            print(pretty_json_text(result), file=sys.stderr, end="")
            return exc.exit_code
        except (ConnectionError, OSError, socket.timeout) as exc:
            error = BusinessError(
                E_NETWORK,
                "worker connection failed",
                {"error": repr(exc), "last_step": self.last_step},
            )
            result.update({"status": "ERROR", "timestamp_utc": utc_timestamp(), **error.to_dict()})
            _write_worker_result(self.config.run_dir, self.worker_id, result)
            return error.exit_code
        finally:
            if self.sock is not None:
                try:
                    self.sock.close()
                except OSError:
                    pass


class _InjectedDisconnect(Exception):
    def __init__(self, step: int) -> None:
        super().__init__(f"injected disconnect after step {step}")
        self.step = step


def run_worker(
    config_path: str | Path,
    worker_id: str,
    shard_id: int,
    standby_file: Optional[str] = None,
    takeover: bool = False,
    resume_consumed_samples: Optional[int] = None,
    known_layout_id: Optional[str] = None,
    inject_bad_gradient_shape: bool = False,
    run_dir_override: Optional[str] = None,
) -> int:
    try:
        config = load_config(config_path, run_dir_override=run_dir_override)
        if standby_file is not None:
            _wait_for_trigger(Path(standby_file), timeout_s=config.registration_timeout_s + 120.0)
        role = "takeover" if takeover or standby_file is not None else "initial"
        worker = TrainingWorker(
            config=config,
            worker_id=worker_id,
            shard_id=shard_id,
            role=role,
            resume_consumed_samples=resume_consumed_samples,
            known_layout_id=known_layout_id,
            inject_bad_gradient_shape=inject_bad_gradient_shape,
        )
        return worker.run()
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), file=sys.stderr, end="")
        return exc.exit_code
    except KeyboardInterrupt:
        return 130


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Run one deterministic MLP training worker")
    parser.add_argument("--config", required=True)
    parser.add_argument("--worker-id", required=True)
    parser.add_argument("--shard-id", required=True, type=int)
    parser.add_argument("--standby-file")
    parser.add_argument("--takeover", action="store_true")
    parser.add_argument("--resume-consumed-samples", type=int)
    parser.add_argument("--known-layout-id")
    parser.add_argument("--inject-bad-gradient-shape", action="store_true")
    parser.add_argument("--run-dir")
    args = parser.parse_args(argv)
    return run_worker(
        args.config,
        args.worker_id,
        args.shard_id,
        standby_file=args.standby_file,
        takeover=args.takeover,
        resume_consumed_samples=args.resume_consumed_samples,
        known_layout_id=args.known_layout_id,
        inject_bad_gradient_shape=args.inject_bad_gradient_shape,
        run_dir_override=args.run_dir,
    )


if __name__ == "__main__":
    raise SystemExit(main())
