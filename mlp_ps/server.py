from __future__ import annotations

import argparse
import asyncio
import json
import os
import socket
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Set, Tuple

import torch

from .checkpoint import (
    CheckpointInfo,
    CheckpointManager,
    CheckpointSnapshot,
    capture_snapshot,
    find_latest_checkpoint,
    pack_shard_state,
)
from .config import Config, SHARD_COUNT, load_config
from .data import eval_dataset, local_batch, train_dataset
from .determinism import configure_deterministic_runtime
from .errors import (
    BusinessError,
    E_CONFIG,
    E_CKPT_CORRUPT,
    E_CKPT_MISSING,
    E_INTERNAL,
    E_LAYOUT_CONFLICT,
    E_NETWORK,
    E_PARAM_SHAPE,
    E_PROGRESS_RANGE,
    E_PROTOCOL,
    E_REGISTRATION_TIMEOUT,
    E_TRAIN_STALL,
    EXIT_CODES,
)
from .layout import Layout, LayoutEntry, build_layout, parameter_shapes_from_spec
from .model import (
    assign_gradients,
    average_gradients,
    build_model,
    build_optimizer,
    evaluate_loss,
    full_gradient_snapshot,
    gradient_norm,
    load_model_state,
    model_state_by_name,
    ordered_parameter_names,
    parameter_shapes,
    zero_gradients,
)
from .protocol import Frame, read_frame, write_frame
from .recovery import RecoveryWAL
from .tensor_codec import pack_named_tensors, tensor_to_bytes, unpack_named_tensors
from .util import (
    atomic_write_bytes,
    atomic_write_text,
    canonical_json_text,
    monotonic_millis,
    pretty_json_text,
    sha256_file,
    utc_timestamp,
)


PROTOCOL_VERSION = 1


@dataclass
class Session:
    worker_id: str
    shard_id: int
    role: str
    reader: asyncio.StreamReader
    writer: asyncio.StreamWriter
    activation_step: Optional[int] = None
    checkpoint_info: Optional[CheckpointInfo] = None
    checkpoint_manifest_sha256: Optional[str] = None
    closed: bool = False
    send_lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    closed_event: asyncio.Event = field(default_factory=asyncio.Event)

    async def send(
        self,
        header: Mapping[str, Any],
        payload: bytes = b"",
        timeout_s: Optional[float] = None,
    ) -> None:
        async with self.send_lock:
            if self.closed:
                raise ConnectionError("session is closed")
            await write_frame(self.writer, header, payload, timeout_s=timeout_s)

    async def receive(
        self,
        max_payload_bytes: int,
        timeout_s: Optional[float] = None,
    ) -> Frame:
        return await read_frame(self.reader, max_payload_bytes, timeout_s=timeout_s)

    async def close(self) -> None:
        if self.closed:
            return
        self.closed = True
        self.closed_event.set()
        try:
            self.writer.close()
            await self.writer.wait_closed()
        except (ConnectionError, OSError, RuntimeError):
            pass


@dataclass
class RoundResult:
    shard_id: int
    worker_id: str
    source: str
    gradients: Dict[str, torch.Tensor]
    consumed_samples: int


class TrainingServer:
    def __init__(self, config: Config) -> None:
        self.config = config
        configure_deterministic_runtime(config.seed)
        self.layout = build_layout(parameter_shapes(
            build_model(config.seed, config.input_dim, config.hidden_dim, config.output_dim)
        ))
        self.model = build_model(config.seed, config.input_dim, config.hidden_dim, config.output_dim)
        self.optimizer = build_optimizer(self.model, config.learning_rate, config.momentum)
        self.parameter_order = list(self.layout.parameter_names)
        self.parameter_shapes = dict(self.layout.parameter_shapes)
        self.train_features, self.train_targets = train_dataset(config)
        self.eval_features, self.eval_targets = eval_dataset(config)
        self.initial_eval_loss = evaluate_loss(
            self.model, self.eval_features, self.eval_targets
        )

        self.run_dir = config.run_dir
        self.run_dir.mkdir(parents=True, exist_ok=True)
        self.status_path = self.run_dir / "status.jsonl"
        self.metrics_path = self.run_dir / "metrics.jsonl"
        self.checkpoint_events_path = self.run_dir / "checkpoint_events.jsonl"
        self.result_path = self.run_dir / "job_result.json"
        self.ready_path = self.run_dir / "server_ready.json"
        self.final_dir = self.run_dir / "final"
        self.status_handle = self.status_path.open("w", encoding="utf-8", newline="\n")
        self.metrics_handle = self.metrics_path.open("w", encoding="utf-8", newline="\n")

        self.checkpoints = CheckpointManager(
            config.checkpoint_dir,
            bytes_per_second=config.checkpoint_bandwidth_mbps * 1_000_000.0,
            padding_bytes=config.checkpoint_padding_bytes,
            max_pending=config.checkpoint_max_pending,
            events_path=self.checkpoint_events_path,
        )
        self.wal = RecoveryWAL()

        self.phase = "registration"
        self.global_step = 0
        self.consumed_samples = {shard_id: 0 for shard_id in range(SHARD_COUNT)}
        self.reserved_initial: Dict[int, Session] = {}
        self.active: Dict[int, Session] = {}
        self.standby: Dict[int, Session] = {}
        self.pending_shards: Set[int] = set()
        self.pending_since_step: Dict[int, int] = {}
        self.missing_consecutive_steps: Dict[int, int] = {}
        self.completed_worker_ids: Dict[int, str] = {}
        self.repair_step_count = 0
        self.pending_step_count = 0
        self.takeover_step: Optional[int] = None
        self.takeover_worker_id: Optional[str] = None
        self.abort_error: Optional[BusinessError] = None
        self.abort_event = asyncio.Event()
        self.registration_event = asyncio.Event()
        self.server: Optional[asyncio.AbstractServer] = None
        self.round_timings: List[Dict[str, Any]] = []
        self.checkpoint_capture_ms: List[float] = []
        self.started_at = time.perf_counter()
        self._finished = False
        self._round_task: Optional[asyncio.Task] = None

        self._bootstrap_checkpoint()

    def _bootstrap_checkpoint(self) -> None:
        snapshot = capture_snapshot(
            self.config,
            self.layout,
            self.model,
            self.optimizer,
            self.consumed_samples,
            self.global_step,
        )
        event = self.checkpoints.write_sync(snapshot)
        if event.get("status") != "OK":
            raise BusinessError(E_INTERNAL, "initial checkpoint failed")
        self._append_wal(self.global_step)

    def _append_wal(self, step: int) -> None:
        state = model_state_by_name(self.model)
        momentum: Dict[str, torch.Tensor] = {}
        for name, param in self.model.named_parameters():
            optimizer_state = self.optimizer.state.get(param, {})
            value = optimizer_state.get("momentum_buffer")
            if value is not None:
                momentum[name] = value.detach().cpu().contiguous()
        packed = {
            shard_id: pack_shard_state(
                self.layout,
                state,
                momentum,
                shard_id,
                self.consumed_samples[shard_id],
            )
            for shard_id in range(SHARD_COUNT)
        }
        self.wal.append(step, packed)

    def _write_jsonl(self, handle: Any, value: Mapping[str, Any]) -> None:
        handle.write(json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n")
        handle.flush()

    def _active_worker_ids(self) -> List[str]:
        return sorted(session.worker_id for session in self.active.values())

    def _full_params_pack(self) -> Tuple[List[Dict[str, Any]], bytes]:
        state = model_state_by_name(self.model)
        return pack_named_tensors(self.parameter_order, state)

    def _owned_params_pack(self, shard_id: int) -> Tuple[List[Dict[str, Any]], bytes]:
        state = model_state_by_name(self.model)
        names = [entry.param_name for entry in self.layout.entries_for_shard(shard_id)]
        slices = {}
        for entry in self.layout.entries_for_shard(shard_id):
            slices[entry.param_name] = state[entry.param_name][
                entry.start : entry.start + entry.length
            ]
        return pack_named_tensors(names, slices)

    def _momentum_pack(self, shard_id: int) -> Tuple[List[Dict[str, Any]], bytes]:
        names: List[str] = []
        tensors: Dict[str, torch.Tensor] = {}
        for entry in self.layout.entries_for_shard(shard_id):
            param = dict(self.model.named_parameters())[entry.param_name]
            state = self.optimizer.state.get(param, {})
            buffer = state.get("momentum_buffer")
            if buffer is None:
                value = torch.zeros(entry.slice_shape, dtype=torch.float32)
            else:
                value = buffer[entry.start : entry.start + entry.length].detach().cpu().contiguous()
            names.append(entry.param_name)
            tensors[entry.param_name] = value
        return pack_named_tensors(names, tensors)

    def _start_message(
        self,
        session: Session,
        checkpoint_info: Optional[CheckpointInfo],
        wal_replayed_steps: int,
    ) -> Tuple[Dict[str, Any], bytes]:
        param_segments, param_payload = self._full_params_pack()
        momentum_segments, momentum_payload = self._momentum_pack(session.shard_id)
        checkpoint_header = None
        if checkpoint_info is not None:
            checkpoint_header = {
                "path": str(checkpoint_info.path),
                "step": checkpoint_info.global_step,
                "layout_id": checkpoint_info.layout_id,
                "manifest_sha256": session.checkpoint_manifest_sha256,
            }
        header = {
            "type": "START",
            "protocol": PROTOCOL_VERSION,
            "worker_id": session.worker_id,
            "shard_id": session.shard_id,
            "global_step": self.global_step,
            "layout_id": self.layout.layout_id,
            "parameter_order": self.parameter_order,
            "param_segments": param_segments,
            "momentum_segments": momentum_segments,
            "consumed_samples": self.consumed_samples[session.shard_id],
            "checkpoint": checkpoint_header,
            "wal_replayed_steps": wal_replayed_steps,
        }
        return header, param_payload + momentum_payload

    async def _send_start_and_wait(
        self,
        session: Session,
        checkpoint_info: Optional[CheckpointInfo],
        wal_replayed_steps: int,
    ) -> None:
        header, payload = self._start_message(
            session, checkpoint_info=checkpoint_info, wal_replayed_steps=wal_replayed_steps
        )
        await session.send(header, payload, timeout_s=self.config.step_timeout_s)
        try:
            frame = await session.receive(
                self.config.max_frame_bytes, timeout_s=self.config.step_timeout_s
            )
        except asyncio.TimeoutError as exc:
            raise ConnectionError("worker did not acknowledge START") from exc
        if frame.message_type == "CLIENT_ERROR":
            code = str(frame.header.get("code", E_PROTOCOL))
            message = str(frame.header.get("message", "worker rejected START"))
            details = frame.header.get("details", {})
            raise BusinessError(code, message, details if isinstance(details, dict) else {})
        if frame.message_type != "READY":
            raise BusinessError(
                E_PROTOCOL,
                "worker did not return READY after START",
                {"worker_id": session.worker_id, "type": frame.message_type},
            )
        if (
            str(frame.header.get("layout_id")) != self.layout.layout_id
            or int(frame.header.get("shard_id", -1)) != session.shard_id
            or str(frame.header.get("worker_id")) != session.worker_id
        ):
            raise BusinessError(E_PROTOCOL, "READY acknowledgement fields do not match")

    async def _register(self, session: Session, request: Mapping[str, Any]) -> None:
        try:
            if int(request.get("protocol", -1)) != PROTOCOL_VERSION:
                raise BusinessError(E_PROTOCOL, "unsupported protocol version")
            requested_shard = int(request.get("shard_id", -1))
            if not (0 <= requested_shard < SHARD_COUNT):
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "worker declared an unknown shard id",
                    {"shard_id": requested_shard},
                )
            role = str(request.get("role", "initial"))
            known_layout = request.get("known_layout_id")
            if known_layout is not None and str(known_layout) != self.layout.layout_id:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "worker declared a stale or unknown layout id",
                    {"declared": known_layout, "expected": self.layout.layout_id},
                )

            if self.phase == "registration":
                if role != "initial":
                    raise BusinessError(E_PROTOCOL, "takeover registration arrived during startup")
                if requested_shard in self.reserved_initial:
                    raise BusinessError(
                        E_LAYOUT_CONFLICT,
                        "two workers declared the same parameter shard",
                        {
                            "shard_id": requested_shard,
                            "workers": [
                                self.reserved_initial[requested_shard].worker_id,
                                session.worker_id,
                            ],
                        },
                    )
                if requested_shard in self.active:
                    raise BusinessError(E_LAYOUT_CONFLICT, "parameter shard is already active")
                self.reserved_initial[requested_shard] = session
                registered = {
                    "type": "REGISTERED",
                    "protocol": PROTOCOL_VERSION,
                    "worker_id": session.worker_id,
                    "shard_id": session.shard_id,
                    "role": role,
                    "global_step": self.global_step,
                    "layout_id": self.layout.layout_id,
                    "layout": self.layout.to_dict(),
                    "activation_step": 0,
                    "checkpoint": None,
                }
                await session.send(registered, timeout_s=self.config.step_timeout_s)
                self.registration_event.set()
                return

            if role != "takeover":
                raise BusinessError(E_PROTOCOL, "late registration must declare takeover role")
            if requested_shard not in self.pending_shards:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "late worker requested a shard that is not pending takeover",
                    {"shard_id": requested_shard, "pending": sorted(self.pending_shards)},
                )
            if requested_shard in self.active:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "two workers simultaneously declare the same active shard",
                    {"shard_id": requested_shard},
                )
            if requested_shard in self.standby:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "a takeover worker is already reserved for this shard",
                    {
                        "shard_id": requested_shard,
                        "workers": [self.standby[requested_shard].worker_id, session.worker_id],
                    },
                )

            checkpoint_info = await asyncio.to_thread(
                find_latest_checkpoint, self.config.checkpoint_dir, True
            )
            if checkpoint_info.layout_id != self.layout.layout_id:
                raise BusinessError(
                    E_CKPT_CORRUPT,
                    "latest checkpoint layout does not match the live layout",
                    {
                        "checkpoint_layout_id": checkpoint_info.layout_id,
                        "layout_id": self.layout.layout_id,
                    },
                )
            manifest_path = checkpoint_info.path / "manifest.json"
            manifest_sha = await asyncio.to_thread(sha256_file, manifest_path)

            requested_cursor = request.get("resume_consumed_samples")
            expected_cursor = self.consumed_samples[requested_shard]
            if requested_cursor is not None and int(requested_cursor) != expected_cursor:
                raise BusinessError(
                    E_PROGRESS_RANGE,
                    "takeover requested a sample cursor that would duplicate or skip progress",
                    {
                        "requested_consumed_samples": int(requested_cursor),
                        "expected_consumed_samples": expected_cursor,
                        "shard_id": requested_shard,
                    },
                )
            if expected_cursor > self.config.sample_count:
                raise BusinessError(
                    E_PROGRESS_RANGE,
                    "takeover shard cursor exceeds the configured sample budget",
                    {
                        "consumed_samples": expected_cursor,
                        "sample_count": self.config.sample_count,
                    },
                )
            if requested_shard not in self.pending_shards:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "shard stopped being pending during takeover validation",
                    {"shard_id": requested_shard},
                )

            pending_since = self.pending_since_step.get(requested_shard, self.global_step + 1)
            activation_step = max(
                pending_since + self.config.fault.outage_steps,
                self.global_step + 1,
            )
            session.activation_step = activation_step
            session.checkpoint_info = checkpoint_info
            session.checkpoint_manifest_sha256 = manifest_sha
            self.standby[requested_shard] = session
            registered = {
                "type": "REGISTERED",
                "protocol": PROTOCOL_VERSION,
                "worker_id": session.worker_id,
                "shard_id": session.shard_id,
                "role": role,
                "global_step": self.global_step,
                "layout_id": self.layout.layout_id,
                "layout": self.layout.to_dict(),
                "activation_step": activation_step,
                "checkpoint": {
                    "path": str(checkpoint_info.path),
                    "step": checkpoint_info.global_step,
                    "layout_id": checkpoint_info.layout_id,
                    "manifest_sha256": manifest_sha,
                },
            }
            await session.send(registered, timeout_s=self.config.step_timeout_s)
            self.registration_event.set()
        except BusinessError as exc:
            await self.abort(exc, extra_sessions=[session])
        except (ConnectionError, OSError, asyncio.TimeoutError):
            await session.close()

    async def _handle_client(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
    ) -> None:
        session: Optional[Session] = None
        try:
            frame = await read_frame(reader, self.config.max_frame_bytes, timeout_s=10.0)
            if frame.message_type != "REGISTER":
                raise BusinessError(E_PROTOCOL, "first client message must be REGISTER")
            worker_id = str(frame.header.get("worker_id", ""))
            shard_id = int(frame.header.get("shard_id", -1))
            role = str(frame.header.get("role", "initial"))
            if not worker_id:
                raise BusinessError(E_PROTOCOL, "worker_id must not be empty")
            session = Session(
                worker_id=worker_id,
                shard_id=shard_id,
                role=role,
                reader=reader,
                writer=writer,
            )
            await self._register(session, frame.header)
            await session.closed_event.wait()
        except BusinessError as exc:
            await self.abort(exc, extra_sessions=[session] if session is not None else [])
        except (ConnectionError, OSError, asyncio.TimeoutError):
            if session is not None:
                await session.close()
        except asyncio.CancelledError:
            if session is not None:
                await session.close()
            raise
        finally:
            if session is not None:
                await session.close()

    async def _wait_for_initial_registrations(self) -> None:
        deadline = time.perf_counter() + self.config.registration_timeout_s
        while len(self.reserved_initial) < SHARD_COUNT:
            if self.abort_error is not None:
                raise self.abort_error
            remaining = deadline - time.perf_counter()
            if remaining <= 0:
                missing = sorted(set(range(SHARD_COUNT)) - set(self.reserved_initial))
                raise BusinessError(
                    E_REGISTRATION_TIMEOUT,
                    "not all four initial workers registered before the timeout",
                    {"missing_shards": missing},
                )
            try:
                await asyncio.wait_for(
                    self.registration_event.wait(), timeout=min(0.1, remaining)
                )
            except asyncio.TimeoutError:
                pass
            self.registration_event.clear()

        for shard_id in range(SHARD_COUNT):
            session = self.reserved_initial[shard_id]
            if session.closed:
                raise BusinessError(
                    E_REGISTRATION_TIMEOUT,
                    "an initial worker disconnected before training started",
                    {"shard_id": shard_id},
                )

    async def _activate_initial(self) -> None:
        self.phase = "training"
        checkpoint_info = find_latest_checkpoint(self.config.checkpoint_dir, validate=True)
        sessions = [self.reserved_initial[shard_id] for shard_id in range(SHARD_COUNT)]
        for shard_id, session in enumerate(sessions):
            self.active[shard_id] = session
            self.completed_worker_ids[shard_id] = session.worker_id
        self.reserved_initial.clear()
        for session in sessions:
            session.checkpoint_info = checkpoint_info
            session.checkpoint_manifest_sha256 = await asyncio.to_thread(
                sha256_file, checkpoint_info.path / "manifest.json"
            )
        try:
            await asyncio.gather(
                *[
                    self._send_start_and_wait(
                        session,
                        checkpoint_info=checkpoint_info,
                        wal_replayed_steps=0,
                    )
                    for session in sessions
                ]
            )
        except (ConnectionError, OSError, asyncio.TimeoutError) as exc:
            raise BusinessError(E_NETWORK, "initial worker activation failed", {"error": str(exc)}) from exc

    async def _promote_standby(self, next_step: int) -> None:
        for shard_id in sorted(list(self.standby.keys())):
            session = self.standby[shard_id]
            activation_step = session.activation_step or (self.global_step + 1)
            if next_step < activation_step:
                continue
            try:
                live_packed = pack_shard_state(
                    self.layout,
                    model_state_by_name(self.model),
                    self._current_momentum_map(),
                    shard_id,
                    self.consumed_samples[shard_id],
                )
                checkpoint_info = session.checkpoint_info
                if checkpoint_info is None:
                    raise BusinessError(E_CKPT_MISSING, "takeover session has no checkpoint anchor")
                recovered = self.wal.recover_shard(
                    shard_id,
                    checkpoint_info.global_step,
                    self.global_step,
                    live_packed.payload,
                    checkpoint_info.shard_bytes.get(shard_id),
                )
                if recovered.consumed_samples >= 0 and recovered.consumed_samples != self.consumed_samples[shard_id]:
                    raise BusinessError(
                        E_PROGRESS_RANGE,
                        "recovery journal sample progress does not match the server cursor",
                        {
                            "shard_id": shard_id,
                            "journal_consumed_samples": recovered.consumed_samples,
                            "server_consumed_samples": self.consumed_samples[shard_id],
                        },
                    )
                if recovered.payload != live_packed.payload:
                    raise BusinessError(
                        E_INTERNAL,
                        "recovered shard state does not match the live state",
                        {"shard_id": shard_id},
                    )
                replayed_steps = self.global_step - checkpoint_info.global_step
                await self._send_start_and_wait(
                    session,
                    checkpoint_info=checkpoint_info,
                    wal_replayed_steps=replayed_steps,
                )
            except BusinessError as exc:
                await self.abort(exc)
                return
            except (ConnectionError, OSError, asyncio.TimeoutError) as exc:
                self.standby.pop(shard_id, None)
                await session.close()
                continue

            self.standby.pop(shard_id, None)
            self.active[shard_id] = session
            self.pending_shards.discard(shard_id)
            self.pending_since_step.pop(shard_id, None)
            self.missing_consecutive_steps[shard_id] = 0
            self.takeover_step = next_step
            self.takeover_worker_id = session.worker_id
            self.completed_worker_ids[shard_id] = session.worker_id

    def _current_momentum_map(self) -> Dict[str, torch.Tensor]:
        momentum: Dict[str, torch.Tensor] = {}
        for name, param in self.model.named_parameters():
            state = self.optimizer.state.get(param, {})
            value = state.get("momentum_buffer")
            if value is not None:
                momentum[name] = value.detach().cpu().contiguous()
        return momentum

    def _mark_disconnected(self, shard_id: int, step: int) -> None:
        session = self.active.pop(shard_id, None)
        if session is not None:
            asyncio.create_task(session.close())
        if shard_id not in self.pending_shards:
            self.pending_shards.add(shard_id)
            self.pending_since_step[shard_id] = step

    def _expected_owned_names(self, shard_id: int) -> List[str]:
        return [entry.param_name for entry in self.layout.entries_for_shard(shard_id)]

    def _expected_owned_shapes(self, shard_id: int) -> Dict[str, Tuple[int, ...]]:
        return {
            entry.param_name: entry.slice_shape
            for entry in self.layout.entries_for_shard(shard_id)
        }

    async def _exchange(
        self,
        session: Session,
        step: int,
        param_segments: Sequence[Mapping[str, Any]],
        param_payload: bytes,
    ) -> Frame:
        data_step = step - 1
        sample_start = data_step * self.config.batch_size + session.shard_id * self.config.local_batch_size
        sample_end = sample_start + self.config.local_batch_size
        header = {
            "type": "TRAIN",
            "protocol": PROTOCOL_VERSION,
            "step": step,
            "layout_id": self.layout.layout_id,
            "worker_id": session.worker_id,
            "shard_id": session.shard_id,
            "sample_start": sample_start,
            "sample_end": sample_end,
            "param_segments": list(param_segments),
        }
        await session.send(header, param_payload, timeout_s=self.config.step_timeout_s)
        frame = await session.receive(
            self.config.max_frame_bytes, timeout_s=self.config.step_timeout_s
        )
        if frame.message_type == "CLIENT_ERROR":
            code = str(frame.header.get("code", E_PROTOCOL))
            message = str(frame.header.get("message", "worker reported an error"))
            details = frame.header.get("details", {})
            raise BusinessError(code, message, details if isinstance(details, dict) else {})
        if frame.message_type != "STEP_RESULT":
            raise BusinessError(
                E_PROTOCOL,
                "worker returned an unexpected message type",
                {"type": frame.message_type, "worker_id": session.worker_id},
            )
        return frame

    def _parse_step_result(
        self,
        session: Session,
        step: int,
        frame: Frame,
        expected_cursor: int,
        param_payload: bytes,
        param_segments: Sequence[Mapping[str, Any]],
    ) -> RoundResult:
        try:
            if int(frame.header.get("step", -1)) != step:
                raise BusinessError(E_PROTOCOL, "worker response step mismatch")
            if int(frame.header.get("shard_id", -1)) != session.shard_id:
                raise BusinessError(E_PROTOCOL, "worker response shard mismatch")
            if str(frame.header.get("layout_id")) != self.layout.layout_id:
                raise BusinessError(E_PROTOCOL, "worker response layout mismatch")
            consumed = int(frame.header.get("consumed_samples_after", -1))
            if consumed != expected_cursor:
                raise BusinessError(
                    E_PROGRESS_RANGE,
                    "worker sample progress is out of range or duplicated",
                    {
                        "shard_id": session.shard_id,
                        "worker_consumed_samples": consumed,
                        "expected_consumed_samples": expected_cursor,
                    },
                )
            gradient_segments = frame.header.get("gradient_segments")
            owned_segments = frame.header.get("owned_param_segments")
            gradient_payload_bytes = int(frame.header.get("gradient_payload_bytes", -1))
            if not isinstance(gradient_segments, list) or not isinstance(owned_segments, list):
                raise BusinessError(E_PARAM_SHAPE, "worker tensor descriptors are missing")
            if gradient_payload_bytes < 0 or gradient_payload_bytes > len(frame.payload):
                raise BusinessError(E_PARAM_SHAPE, "worker gradient payload boundary is invalid")
            gradients = unpack_named_tensors(
                gradient_segments,
                frame.payload[:gradient_payload_bytes],
                self.parameter_order,
                self.parameter_shapes,
            )
            owned = unpack_named_tensors(
                owned_segments,
                frame.payload[gradient_payload_bytes:],
                self._expected_owned_names(session.shard_id),
                self._expected_owned_shapes(session.shard_id),
            )
            for name in gradients:
                if not bool(torch.isfinite(gradients[name]).all().item()):
                    raise BusinessError(
                        E_PARAM_SHAPE,
                        "worker produced a non-finite gradient",
                        {"parameter": name, "shard_id": session.shard_id},
                    )
            broadcast_params = unpack_named_tensors(
                param_segments,
                param_payload,
                self.parameter_order,
                self.parameter_shapes,
            )
            for entry in self.layout.entries_for_shard(session.shard_id):
                expected = broadcast_params[entry.param_name][
                    entry.start : entry.start + entry.length
                ]
                if not torch.equal(owned[entry.param_name], expected):
                    raise BusinessError(
                        E_PROTOCOL,
                        "worker returned a stale or altered shard parameter value",
                        {"parameter": entry.param_name, "shard_id": session.shard_id},
                    )
        except BusinessError:
            raise
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise BusinessError(
                E_PARAM_SHAPE,
                "worker gradient or parameter shape validation failed",
                {"worker_id": session.worker_id, "error": str(exc)},
            ) from exc
        return RoundResult(
            shard_id=session.shard_id,
            worker_id=session.worker_id,
            source="worker",
            gradients=gradients,
            consumed_samples=consumed,
        )

    def _repair_gradients(self, step: int, shard_ids: Sequence[int]) -> Dict[int, Dict[str, torch.Tensor]]:
        result: Dict[int, Dict[str, torch.Tensor]] = {}
        for shard_id in shard_ids:
            zero_gradients(self.model)
            features, targets, _, _ = local_batch(
                self.train_features,
                self.train_targets,
                step - 1,
                shard_id,
                self.config,
            )
            prediction = self.model(features)
            loss = torch.mean((prediction - targets) ** 2)
            loss.backward()
            result[shard_id] = full_gradient_snapshot(self.model)
        zero_gradients(self.model)
        return result

    async def _run_round(self, step: int) -> None:
        round_started = time.perf_counter()
        await self._promote_standby(step)
        if self.abort_error is not None:
            raise self.abort_error

        param_segments, param_payload = self._full_params_pack()
        active_snapshot = dict(self.active)
        exchange_tasks: Dict[int, asyncio.Task] = {}
        for shard_id, session in active_snapshot.items():
            exchange_tasks[shard_id] = asyncio.create_task(
                self._exchange(session, step, param_segments, param_payload)
            )
        collect_started = time.perf_counter()
        shard_order = sorted(exchange_tasks)
        frames = await asyncio.gather(
            *[exchange_tasks[shard_id] for shard_id in shard_order],
            return_exceptions=True,
        )
        collect_ended = time.perf_counter()

        results: Dict[int, RoundResult] = {}
        protocol_error: Optional[BusinessError] = None
        for shard_id, frame in zip(shard_order, frames):
            session = active_snapshot[shard_id]
            if isinstance(frame, BaseException):
                if isinstance(frame, BusinessError):
                    protocol_error = frame
                self._mark_disconnected(shard_id, step)
                continue
            expected_cursor = self.consumed_samples[shard_id] + self.config.local_batch_size
            try:
                results[shard_id] = self._parse_step_result(
                    session,
                    step,
                    frame,
                    expected_cursor,
                    param_payload,
                    param_segments,
                )
            except BusinessError as exc:
                protocol_error = exc
                self._mark_disconnected(shard_id, step)

        if protocol_error is not None:
            await self.abort(protocol_error)
            raise protocol_error

        missing_shards = [
            shard_id for shard_id in range(SHARD_COUNT) if shard_id not in results
        ]
        if missing_shards:
            repair_started = time.perf_counter()
            repair_gradients = await asyncio.to_thread(
                self._repair_gradients, step, missing_shards
            )
            repair_elapsed_ms = (time.perf_counter() - repair_started) * 1000.0
            for shard_id in missing_shards:
                results[shard_id] = RoundResult(
                    shard_id=shard_id,
                    worker_id="server-repair",
                    source="repair",
                    gradients=repair_gradients[shard_id],
                    consumed_samples=self.consumed_samples[shard_id]
                    + self.config.local_batch_size,
                )
                self.repair_step_count += 1
        else:
            repair_elapsed_ms = 0.0

        aggregate_started = time.perf_counter()
        graded = [results[shard_id] for shard_id in range(SHARD_COUNT)]
        averaged = average_gradients([item.gradients for item in graded], self.parameter_order)
        norm = gradient_norm(averaged)
        assign_gradients(self.model, averaged)
        self.optimizer.step()
        zero_gradients(self.model)
        for item in graded:
            self.consumed_samples[item.shard_id] = item.consumed_samples
        self.global_step = step
        self._append_wal(step)
        aggregate_ended = time.perf_counter()

        for shard_id in range(SHARD_COUNT):
            if results[shard_id].source == "repair":
                self.pending_step_count += 1
                self.missing_consecutive_steps[shard_id] = (
                    self.missing_consecutive_steps.get(shard_id, 0) + 1
                )
            else:
                self.missing_consecutive_steps[shard_id] = 0

        status = {
            "schema_version": 1,
            "step": step,
            "layout_id": self.layout.layout_id,
            "global_batch_size": self.config.batch_size,
            "consumed_samples_total": step * self.config.batch_size,
            "worker_consumed_samples": {
                str(shard_id): self.consumed_samples[shard_id]
                for shard_id in range(SHARD_COUNT)
            },
            "active_worker_count": len(self.active),
            "active_worker_ids": self._active_worker_ids(),
            "pending_shards": sorted(self.pending_shards),
            "submissions": [
                {
                    "shard_id": shard_id,
                    "worker_id": results[shard_id].worker_id,
                    "submission_seq": step * SHARD_COUNT + shard_id,
                    "source": results[shard_id].source,
                    "consumed_samples": results[shard_id].consumed_samples,
                }
                for shard_id in range(SHARD_COUNT)
            ],
            "aggregate_gradient_norm": round(norm, 12),
        }
        self._write_jsonl(self.status_handle, status)

        checkpoint_capture_ms: Optional[float] = None
        if step in set(self.config.checkpoint_steps):
            capture_started = time.perf_counter()
            snapshot = capture_snapshot(
                self.config,
                self.layout,
                self.model,
                self.optimizer,
                self.consumed_samples,
                step,
            )
            checkpoint_capture_ms = (time.perf_counter() - capture_started) * 1000.0
            self.checkpoint_capture_ms.append(checkpoint_capture_ms)
            self.checkpoints.submit(snapshot)

        checkpoint_error = self.checkpoints.first_error()
        if checkpoint_error is not None:
            raise BusinessError(
                E_INTERNAL,
                "asynchronous checkpoint writer failed",
                {"error": repr(checkpoint_error)},
            )

        round_ended = time.perf_counter()
        timing = {
            "timestamp_utc": utc_timestamp(),
            "step": step,
            "round_ms": round((round_ended - round_started) * 1000.0, 6),
            "collect_ms": round((collect_ended - collect_started) * 1000.0, 6),
            "repair_ms": round(repair_elapsed_ms, 6),
            "aggregate_ms": round((aggregate_ended - aggregate_started) * 1000.0, 6),
            "active_worker_count": len(self.active),
            "repair_shards": missing_shards,
            "checkpoint_pending": self.checkpoints.pending_count(),
        }
        if checkpoint_capture_ms is not None:
            timing["checkpoint_capture_ms"] = round(checkpoint_capture_ms, 6)
        self.round_timings.append(timing)
        self._write_jsonl(self.metrics_handle, timing)

        for shard_id in sorted(self.pending_shards):
            if self.missing_consecutive_steps.get(shard_id, 0) > self.config.fault.stall_tolerance_steps:
                raise BusinessError(
                    E_TRAIN_STALL,
                    "disconnected shard exceeded the configured takeover tolerance",
                    {
                        "shard_id": shard_id,
                        "missing_steps": self.missing_consecutive_steps[shard_id],
                        "tolerance_steps": self.config.fault.stall_tolerance_steps,
                    },
                )

        if self.config.step_pace_ms > 0:
            await asyncio.sleep(self.config.step_pace_ms / 1000.0)

    async def abort(
        self,
        error: BusinessError,
        extra_sessions: Optional[Sequence[Optional[Session]]] = None,
    ) -> None:
        if self.abort_error is None:
            self.abort_error = error
        self.abort_event.set()
        sessions: List[Session] = []
        seen = set()
        for session in list(self.active.values()) + list(self.reserved_initial.values()) + list(
            self.standby.values()
        ):
            if id(session) not in seen:
                seen.add(id(session))
                sessions.append(session)
        for session in extra_sessions or []:
            if session is not None and id(session) not in seen:
                seen.add(id(session))
                sessions.append(session)
        abort_header = {
            "type": "ABORT",
            "protocol": PROTOCOL_VERSION,
            "code": error.code,
            "message": error.message,
            "details": error.details,
        }

        async def _send_abort(session: Session) -> None:
            try:
                await session.send(
                    abort_header,
                    timeout_s=self.config.abort_timeout_s,
                )
            except (ConnectionError, OSError, RuntimeError, asyncio.TimeoutError):
                pass

        if sessions:
            await asyncio.gather(
                *[_send_abort(session) for session in sessions],
                return_exceptions=True,
            )
            await asyncio.gather(
                *[session.close() for session in sessions],
                return_exceptions=True,
            )

    def _export_final_state(self) -> Dict[str, Any]:
        self.final_dir.mkdir(parents=True, exist_ok=True)
        state = model_state_by_name(self.model)
        segments, payload = pack_named_tensors(self.parameter_order, state)
        atomic_write_bytes(self.final_dir / "params.bin", payload)
        index = {
            "format": "mlp-ps-final-v1",
            "global_step": self.global_step,
            "layout_id": self.layout.layout_id,
            "layout": self.layout.spec,
            "parameter_order": self.parameter_order,
            "parameter_shapes": {
                name: list(shape) for name, shape in self.parameter_shapes.items()
            },
            "segments": segments,
            "consumed_samples": {
                str(shard_id): self.consumed_samples[shard_id]
                for shard_id in range(SHARD_COUNT)
            },
            "data_bytes": len(payload),
            "sha256": sha256_file(self.final_dir / "params.bin"),
        }
        atomic_write_text(self.final_dir / "index.json", pretty_json_text(index))
        return index

    def _summary(self) -> Dict[str, Any]:
        round_ms = [float(item["round_ms"]) for item in self.round_timings]
        collect_ms = [float(item["collect_ms"]) for item in self.round_timings]
        aggregate_ms = [float(item["aggregate_ms"]) for item in self.round_timings]
        summary = {
            "status": "OK",
            "event": "COMPLETED",
            "timestamp_utc": utc_timestamp(),
            "global_step": self.global_step,
            "total_steps": self.config.total_steps,
            "layout_id": self.layout.layout_id,
            "config_fingerprint": self.config.config_fingerprint,
            "active_worker_ids": self._active_worker_ids(),
            "takeover_step": self.takeover_step,
            "takeover_worker_id": self.takeover_worker_id,
            "repair_submissions": self.repair_step_count,
            "steps_with_pending_shards": self.pending_step_count,
            "round_count": len(round_ms),
            "average_round_ms": round(sum(round_ms) / len(round_ms), 6) if round_ms else 0.0,
            "max_round_ms": round(max(round_ms), 6) if round_ms else 0.0,
            "average_broadcast_collect_ms": round(
                sum(collect_ms) / len(collect_ms), 6
            )
            if collect_ms
            else 0.0,
            "average_aggregate_ms": round(sum(aggregate_ms) / len(aggregate_ms), 6)
            if aggregate_ms
            else 0.0,
            "max_checkpoint_capture_ms": round(max(self.checkpoint_capture_ms), 6)
            if self.checkpoint_capture_ms
            else 0.0,
            "initial_eval_mse": self.initial_eval_loss,
            "final_eval_mse": evaluate_loss(self.model, self.eval_features, self.eval_targets),
            "elapsed_seconds": round(time.perf_counter() - self.started_at, 6),
            "consumed_samples_total": self.global_step * self.config.batch_size,
        }
        return summary

    def _write_result(self, result: Mapping[str, Any]) -> None:
        atomic_write_text(self.result_path, pretty_json_text(result))

    async def _finish(self) -> None:
        self._export_final_state()
        self.checkpoints.flush(timeout_s=max(60.0, self.config.total_steps * 0.5))
        summary = self._summary()
        shutdown_header = {
            "type": "SHUTDOWN",
            "protocol": PROTOCOL_VERSION,
            "global_step": self.global_step,
            "layout_id": self.layout.layout_id,
            "summary": {
                "final_eval_mse": summary["final_eval_mse"],
                "repair_submissions": summary["repair_submissions"],
                "takeover_step": summary["takeover_step"],
            },
        }
        sessions = list(self.active.values())

        async def _shutdown(session: Session) -> None:
            try:
                await session.send(shutdown_header, timeout_s=self.config.abort_timeout_s)
            except (ConnectionError, OSError, RuntimeError, asyncio.TimeoutError):
                pass

        if sessions:
            await asyncio.gather(*[_shutdown(session) for session in sessions], return_exceptions=True)
        await asyncio.gather(*[session.close() for session in sessions], return_exceptions=True)
        self.active.clear()
        self._write_result(summary)
        self._finished = True

    async def run(self) -> int:
        try:
            self.server = await asyncio.start_server(
                self._handle_client,
                host=self.config.host,
                port=self.config.port,
                start_serving=True,
            )
        except OSError as exc:
            error = BusinessError(
                E_NETWORK,
                "failed to bind the parameter-server TCP port",
                {"host": self.config.host, "port": self.config.port, "error": str(exc)},
            )
            self._write_result({"status": "ERROR", **error.to_dict()})
            return error.exit_code
        ready = {
            "status": "READY",
            "timestamp_utc": utc_timestamp(),
            "pid": os.getpid(),
            "host": self.config.host,
            "port": self.config.port,
            "layout_id": self.layout.layout_id,
            "layout": self.layout.to_dict(),
            "checkpoint_steps": self.config.checkpoint_steps,
        }
        atomic_write_text(self.ready_path, pretty_json_text(ready))

        exit_code = 0
        try:
            await self._wait_for_initial_registrations()
            await self._activate_initial()
            for step in range(1, self.config.total_steps + 1):
                if self.abort_error is not None:
                    raise self.abort_error
                self._round_task = asyncio.create_task(self._run_round(step))
                await self._round_task
                if self.abort_error is not None:
                    raise self.abort_error
            await self._finish()
        except BusinessError as exc:
            await self.abort(exc)
            exit_code = exc.exit_code
            self._write_result({"status": "ERROR", "event": "ABORTED", **exc.to_dict()})
        except asyncio.CancelledError:
            error = BusinessError(E_INTERNAL, "parameter server was cancelled")
            await self.abort(error)
            exit_code = error.exit_code
            self._write_result({"status": "ERROR", "event": "ABORTED", **error.to_dict()})
            raise
        except Exception as exc:
            error = BusinessError(E_INTERNAL, "unhandled parameter-server failure", {"error": repr(exc)})
            await self.abort(error)
            exit_code = error.exit_code
            self._write_result({"status": "ERROR", "event": "ABORTED", **error.to_dict()})
        finally:
            if self.server is not None:
                self.server.close()
                await self.server.wait_closed()
            if not self._finished:
                try:
                    self.checkpoints.close()
                except Exception:
                    pass
            else:
                self.checkpoints.close()
            self.status_handle.close()
            self.metrics_handle.close()
        return exit_code


def _remove_run_dir_contents(run_dir: Path) -> None:
    if not run_dir.exists():
        return
    for child in run_dir.iterdir():
        if child.is_dir():
            import shutil

            shutil.rmtree(child)
        else:
            child.unlink()


def run_server(config_path: str | Path, run_dir_override: Optional[str] = None, clean: bool = False) -> int:
    try:
        config = load_config(config_path, run_dir_override=run_dir_override)
        if clean:
            _remove_run_dir_contents(config.run_dir)
        server = TrainingServer(config)
        return asyncio.run(server.run())
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), file=sys.stderr, end="")
        return exc.exit_code
    except KeyboardInterrupt:
        return 130


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Run the deterministic parameter server")
    parser.add_argument("--config", required=True)
    parser.add_argument("--run-dir")
    parser.add_argument("--clean", action="store_true")
    args = parser.parse_args(argv)
    return run_server(args.config, run_dir_override=args.run_dir, clean=args.clean)


if __name__ == "__main__":
    raise SystemExit(main())
