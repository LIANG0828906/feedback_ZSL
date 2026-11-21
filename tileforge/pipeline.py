"""End-to-end incremental conversion, publication, offline output and recovery."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .binary import (
    atomic_write_bytes,
    compute_block_hash,
    make_block,
    parse_sequence,
    serialize_sequence,
    write_sequence_atomic,
)
from .budget import estimate_tile_memory
from .color import ColorPipeline, TileResult, convert_tile, tile_fingerprint
from .config import JobConfig
from .errors import (
    E_BUDGET_EXCEEDED,
    E_CANCELLED,
    E_RECOVERY_INVALID,
    E_VERSION_STALE,
    TileForgeError,
    error,
)
from .locks import RunLock
from .model import SequenceHeader, TileBlock, TileSequence
from .recovery import REQUIRED_STATE_FIELDS, RecoveryJournal
from .scheduler import ConversionScheduler
from .store import Snapshot, VersionStore
from .trace import TraceLog


def _canonical(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False)


def _payload_hash(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _unique_trace_path(path: Path) -> Path:
    if not path.exists():
        return path
    index = 1
    while True:
        candidate = path.with_name(f"{path.stem}.{index}{path.suffix}")
        if not candidate.exists():
            return candidate
        index += 1


def _tile_record(
    block: TileBlock,
    fingerprint: str,
    source_hash: str,
    max_code_delta: int,
    profile: str,
    origin_version: int | None,
    reused: bool,
    parameter_version: int,
) -> dict[str, Any]:
    return {
        "tile_id": block.tile_id,
        "fingerprint": fingerprint,
        "source_hash": source_hash,
        "content_hash": block.content_hash.hex(),
        "payload_hash": _payload_hash(block.data),
        "max_code_delta": max_code_delta,
        "profile": profile,
        "downgrade_stage": profile,
        "origin_version": origin_version,
        "parameter_version": parameter_version,
        "reused": reused,
    }


def _offline_tiles(sequence: TileSequence, tile_records: list[dict[str, Any]]) -> list[dict[str, Any]]:
    by_id = sequence.block_map()
    records = []
    for record in sorted(tile_records, key=lambda item: int(item["tile_id"])):
        block = by_id[int(record["tile_id"])]
        records.append(
            {
                "tile_id": block.tile_id,
                "content_hash": block.content_hash.hex(),
                "payload_hash": _payload_hash(block.data),
                "payload_b64": base64.b64encode(block.data).decode("ascii"),
                "sample_count": block.sample_count,
                "exposure_stops": block.exposure,
                "max_code_delta": record["max_code_delta"],
                "downgrade_stage": record["profile"],
            }
        )
    return records


def _build_output_sequence(
    input_sequence: TileSequence,
    config: JobConfig,
    outputs: dict[int, TileBlock],
    base_sequence: TileSequence | None,
    reused_tiles: set[int],
) -> TileSequence:
    blocks: list[TileBlock] = []
    base_blocks = base_sequence.block_map() if base_sequence is not None else {}
    for tile_id in input_sequence.tile_ids():
        if tile_id in reused_tiles:
            if tile_id not in base_blocks:
                raise error(E_VERSION_STALE, "reused tile is absent from the base version", field="tile_id", tile_id=tile_id)
            blocks.append(base_blocks[tile_id])
        else:
            blocks.append(outputs[tile_id])
    header = SequenceHeader(
        width=input_sequence.header.width,
        height=input_sequence.header.height,
        tiles_x=input_sequence.header.tiles_x,
        tiles_y=input_sequence.header.tiles_y,
        bit_depth=input_sequence.header.bit_depth,
        channels=input_sequence.header.channels,
        source_transfer=config.source.transfer,
        target_transfer=config.target.transfer,
        conversion_id=config.conversion_id(),
        block_count=len(blocks),
        data_length=sum(block.payload_size for block in blocks),
        flags=input_sequence.header.flags,
    )
    return TileSequence(header=header, blocks=sorted(blocks, key=lambda block: block.tile_id))


def _load_base(
    store: VersionStore,
    input_sequence: TileSequence,
    config: JobConfig,
    expected_base_hash: str | None,
) -> Snapshot | None:
    if expected_base_hash is not None:
        base = store.load_current(expected_base_hash)
    else:
        try:
            base = store.load_current()
        except TileForgeError as exc:
            if exc.code == E_VERSION_STALE and exc.message == "no version has been published":
                return None
            raise
    return base


def _load_recovery_base(
    store: VersionStore,
    journal_root: Path,
    *,
    input_hash: str,
    config: JobConfig,
    params_version: int,
) -> tuple[Snapshot | None, bool]:
    state_path = journal_root / "state.json"
    if not state_path.exists():
        return None, False
    try:
        state = json.loads(state_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError, UnicodeError) as exc:
        raise error(
            E_RECOVERY_INVALID,
            "cannot read recovery state",
            field="state.json",
            path=str(state_path),
            reason=str(exc),
        ) from exc
    if not isinstance(state, dict):
        raise error(E_RECOVERY_INVALID, "recovery state is not an object", field="state.json")
    missing = REQUIRED_STATE_FIELDS - state.keys()
    if missing:
        raise error(
            E_RECOVERY_INVALID,
            "recovery state is missing fields",
            field="state.json",
            missing=sorted(missing),
        )
    for field_name, expected in (
        ("input_hash", input_hash),
        ("config_hash", config.config_hash()),
        ("sync_marker", config.observation.sync_marker),
        ("params_version", params_version),
    ):
        if state.get(field_name) != expected:
            raise error(
                E_RECOVERY_INVALID,
                "recovery state parameter version mismatch",
                field=field_name,
                actual=state.get(field_name),
                expected=expected,
            )
    base_hash = state.get("base_version_hash")
    base_relative = state.get("base_manifest_path")
    if base_hash is None and base_relative is None:
        return None, True
    if not isinstance(base_hash, str) or not isinstance(base_relative, str):
        raise error(
            E_RECOVERY_INVALID,
            "recovery base version fields are inconsistent",
            field="base_version_hash/base_manifest_path",
            actual=[base_hash, base_relative],
        )
    try:
        manifest = store.load_manifest_relative(base_relative)
        base = store.load_snapshot_from_manifest(manifest)
    except TileForgeError as exc:
        raise error(
            E_RECOVERY_INVALID,
            "recovery base version cannot be loaded",
            field="base_manifest_path",
            reason=exc.to_dict(),
        ) from exc
    if base.version_hash != base_hash:
        raise error(
            E_RECOVERY_INVALID,
            "recovery base version hash mismatch",
            field="base_version_hash",
            actual=base.version_hash,
            expected=base_hash,
        )
    return base, True


@dataclass
class ConversionSummary:
    version_id: int | None
    version_hash: str
    affected_tiles: list[int]
    reused_tiles: list[int]
    recomputed_tiles: list[int]
    downgrade_stages: dict[int, str]
    content_hashes: dict[int, str]
    max_code_deltas: dict[int, int]
    trace_path: str | None
    output_path: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": True,
            "version_id": self.version_id,
            "version_hash": self.version_hash,
            "affected_count": len(self.affected_tiles),
            "affected_tiles": self.affected_tiles,
            "reused_count": len(self.reused_tiles),
            "reused_tiles": self.reused_tiles,
            "recomputed_count": len(self.recomputed_tiles),
            "recomputed_tiles": self.recomputed_tiles,
            "downgrade_stages": {str(key): value for key, value in sorted(self.downgrade_stages.items())},
            "content_hashes": {str(key): value for key, value in sorted(self.content_hashes.items())},
            "max_code_deltas": {str(key): value for key, value in sorted(self.max_code_deltas.items())},
            "trace_path": self.trace_path,
            "output_path": self.output_path,
        }


class ConversionJob:
    """Background conversion controller used by the GUI."""

    def __init__(
        self,
        input_path: str | Path,
        config: JobConfig,
        workspace: str | Path,
        *,
        expected_base_hash: str | None = None,
        resume: bool = True,
    ) -> None:
        self.input_path = Path(input_path)
        self.config = config
        self.workspace = Path(workspace)
        self.expected_base_hash = expected_base_hash
        self.resume = resume
        self._thread: threading.Thread | None = None
        self._scheduler: ConversionScheduler | None = None
        self._scheduler_lock = threading.Lock()
        self._cancel_requested = False
        self.result: dict[str, Any] | None = None
        self.error: BaseException | None = None

    def _attach_scheduler(self, scheduler: ConversionScheduler) -> None:
        with self._scheduler_lock:
            self._scheduler = scheduler
            if self._cancel_requested:
                scheduler.cancel()

    def start(self) -> None:
        if self._thread is not None:
            raise error(E_CANCELLED, "conversion job already started", field="job")

        def runner() -> None:
            try:
                summary = convert_incremental(
                    self.input_path,
                    self.config,
                    self.workspace,
                    expected_base_hash=self.expected_base_hash,
                    resume=self.resume,
                    scheduler_ready=self._attach_scheduler,
                )
                self.result = summary.to_dict()
            except BaseException as exc:
                self.error = exc

        self._thread = threading.Thread(target=runner, name="tileforge-job", daemon=True)
        self._thread.start()

    def pause(self) -> None:
        with self._scheduler_lock:
            if self._scheduler is not None:
                self._scheduler.pause()

    def resume(self) -> None:
        with self._scheduler_lock:
            if self._scheduler is not None:
                self._scheduler.resume()

    def cancel(self) -> None:
        self._cancel_requested = True
        with self._scheduler_lock:
            if self._scheduler is not None:
                self._scheduler.cancel()

    def join(self, timeout: float | None = None) -> dict[str, Any] | None:
        if self._thread is not None:
            self._thread.join(timeout)
        if self.error is not None:
            raise self.error
        return self.result

    @property
    def is_alive(self) -> bool:
        return self._thread is not None and self._thread.is_alive()


def convert_incremental(
    input_path: str | Path,
    config: JobConfig,
    workspace: str | Path,
    *,
    expected_base_hash: str | None = None,
    resume: bool = True,
    restart: bool = False,
    trace_path: str | Path | None = None,
    crash_after_commits: int | None = None,
    scheduler_ready: Callable[[ConversionScheduler], None] | None = None,
    on_commit: Callable[[int, str], None] | None = None,
) -> ConversionSummary:
    source_path = Path(input_path)
    input_sequence = parse_sequence(source_path)
    config.validate_for_sequence(
        input_sequence.header.width,
        input_sequence.header.height,
        input_sequence.header.block_count,
        input_sequence.header.source_transfer,
        input_sequence.header.conversion_id,
    )
    store = VersionStore(workspace)
    input_hash = input_sequence.file_sha256 or _payload_hash(source_path.read_bytes())
    lock_path = store.root / "locks" / f"run-{input_hash[:32]}.lock"
    with RunLock(lock_path, input_hash):
        # Parameter versions are derived from canonical parameters, not from a
        # global publication counter, so another sequence publishing between a
        # crash and recovery cannot invalidate this journal.
        params_version = int(config.config_hash()[:8], 16) & 0x7FFFFFFF
        journal_root = store.runs_dir / (
            "run-"
            + hashlib.sha256(
                _canonical(
                    {
                        "input_hash": input_hash,
                        "config_hash": config.config_hash(),
                        "sync_marker": config.observation.sync_marker,
                        "parameter_version": params_version,
                    }
                ).encode("utf-8")
            ).hexdigest()[:24]
        )
        if expected_base_hash is not None:
            store.load_current(expected_base_hash)
        base, has_recovery_state = (None, False)
        if resume and not restart:
            base, has_recovery_state = _load_recovery_base(
                store,
                journal_root,
                input_hash=input_hash,
                config=config,
                params_version=params_version,
            )
        if not has_recovery_state:
            base = _load_base(store, input_sequence, config, expected_base_hash)
        trace_file = (
            Path(trace_path)
            if trace_path is not None
            else _unique_trace_path(
                store.traces_dir
                / f"run-{input_hash[:12]}-{config.config_hash()[:12]}-p{params_version:010d}.jsonl"
            )
        )
        trace = TraceLog(trace_file)
        trace.emit(
            "conversion_request",
            input_path=str(source_path),
            input_hash=input_hash,
            config_hash=config.config_hash(),
            conversion_id=config.conversion_id(),
            sync_marker=config.observation.sync_marker,
            parameter_version=params_version,
            base_version_id=base.version_id if base else None,
            base_version_hash=base.version_hash if base else None,
        )
        old_records: dict[int, dict[str, Any]] = {}
        can_reuse = (
            base is not None
            and base.manifest.get("input_hash") == input_hash
            and base.sequence.header.width == input_sequence.header.width
            and base.sequence.header.height == input_sequence.header.height
            and base.sequence.header.tiles_x == input_sequence.header.tiles_x
            and base.sequence.header.tiles_y == input_sequence.header.tiles_y
            and base.sequence.header.bit_depth == input_sequence.header.bit_depth
            and base.sequence.header.channels == input_sequence.header.channels
        )
        if can_reuse:
            old_records = {int(record["tile_id"]): record for record in base.manifest.get("tile_records", [])}
            if set(old_records) != set(input_sequence.tile_ids()):
                can_reuse = False

        fingerprints: dict[int, str] = {}
        affected_tiles: list[int] = []
        for block in input_sequence.ordered_blocks():
            fingerprint = tile_fingerprint(input_sequence, block, config)
            fingerprints[block.tile_id] = fingerprint
            old = old_records.get(block.tile_id)
            if not can_reuse or old is None or old.get("fingerprint") != fingerprint:
                affected_tiles.append(block.tile_id)
        affected_set = set(affected_tiles)
        reused_tiles = [tile_id for tile_id in input_sequence.tile_ids() if tile_id not in affected_set]
        trace.emit(
            "incremental_plan",
            affected_tiles=affected_tiles,
            affected_count=len(affected_tiles),
            reused_tiles=reused_tiles,
            reused_count=len(reused_tiles),
            recomputed_count=len(affected_tiles),
        )

        journal = RecoveryJournal(
            journal_root,
            input_hash=input_hash,
            config_hash=config.config_hash(),
            sync_marker=config.observation.sync_marker,
            affected_tile_ids=affected_tiles,
            base_version_hash=base.version_hash if base else None,
            base_manifest_path=(
                f"manifests/v{base.version_id:012d}.json" if base is not None else None
            ),
            params_version=params_version,
        )
        journal.initialize(restart=restart)
        recovered_existing = journal.load_valid()
        if recovered_existing and not resume:
            raise error(
                E_RECOVERY_INVALID,
                "existing recovery commits require resume or restart",
                field="recovery",
                run_key=journal.run_key,
            )
        recovered = recovered_existing if resume else []
        recovered_by_id = {item.tile_id: item for item in recovered}
        previous_chain = recovered[-1].commit_hash if recovered else journal.chain_seed
        next_seq = len(recovered)
        trace.emit(
            "recovery_scan",
            run_key=journal.run_key,
            parameter_version=params_version,
            recovered_tiles=[item.tile_id for item in recovered],
            recovered_count=len(recovered),
            next_seq=next_seq,
        )

        pipeline = ColorPipeline.from_config(config)
        outputs: dict[int, TileBlock] = {}
        result_meta: dict[int, dict[str, Any]] = {}
        downgrade_stages: dict[int, str] = {}
        commit_count = 0

        def recovered_block(item: Any, block: TileBlock) -> TileBlock:
            expected_content = compute_block_hash(
                block.tile_id,
                block.x,
                block.y,
                block.width,
                block.height,
                item.exposure,
                block.sample_count,
                item.payload,
            ).hex()
            if expected_content != item.content_hash:
                raise error("E_RECOVERY_INVALID", "recovered block content hash mismatch", field="content_hash", tile_id=block.tile_id)
            if _payload_hash(item.payload) != item.output_hash:
                raise error("E_RECOVERY_INVALID", "recovered block payload hash mismatch", field="output_hash", tile_id=block.tile_id)
            rebuilt = make_block(
                block.tile_id,
                block.x,
                block.y,
                block.width,
                block.height,
                item.exposure,
                input_sequence.header.channels,
                item.payload,
            )
            if rebuilt.content_hash.hex() != item.content_hash:
                raise error("E_RECOVERY_INVALID", "recovered block hash is inconsistent", field="content_hash", tile_id=block.tile_id)
            result_meta[block.tile_id] = {
                "max_code_delta": item.max_code_delta,
                "profile": item.profile,
                "usage_bytes": item.usage_bytes,
                "tone_map_curve": item.tone_map_curve,
            }
            downgrade_stages[block.tile_id] = item.profile
            return rebuilt

        input_blocks = input_sequence.block_map()
        for item in recovered:
            outputs[item.tile_id] = recovered_block(item, input_blocks[item.tile_id])

        missing = [tile_id for tile_id in affected_tiles if tile_id not in recovered_by_id]
        scheduler: ConversionScheduler | None = None
        if missing:
            scheduler = ConversionScheduler(input_sequence.header, config.memory, trace)
            if scheduler_ready is not None:
                scheduler_ready(scheduler)

            def task(tile_id: int, profile: str) -> tuple[TileResult, dict[str, Any]]:
                result = convert_tile(input_sequence, input_blocks[tile_id], config, pipeline, profile=profile)
                usage = estimate_tile_memory(
                    input_sequence.header,
                    profile,
                    config.memory.estimator_overhead_bytes,
                ).to_dict()
                return result, usage

            def commit(tile_id: int, value: tuple[TileResult, dict[str, Any]]) -> None:
                nonlocal previous_chain, next_seq, commit_count
                result, usage = value
                previous_chain = journal.commit(
                    seq=next_seq,
                    tile_id=tile_id,
                    submit_order=next_seq,
                    payload=result.data,
                    content_hash=result.content_hash.hex(),
                    exposure=result.exposure,
                    max_code_delta=result.max_code_delta,
                    tone_map_curve=result.tone_map_curve,
                    profile=result.profile,
                    usage_bytes=usage,
                    previous_chain=previous_chain,
                )
                block = make_block(
                    tile_id,
                    input_blocks[tile_id].x,
                    input_blocks[tile_id].y,
                    input_blocks[tile_id].width,
                    input_blocks[tile_id].height,
                    result.exposure,
                    input_sequence.header.channels,
                    result.data,
                )
                outputs[tile_id] = block
                result_meta[tile_id] = {
                    "max_code_delta": result.max_code_delta,
                    "profile": result.profile,
                    "usage_bytes": usage,
                    "tone_map_curve": result.tone_map_curve,
                }
                downgrade_stages[tile_id] = result.profile
                next_seq += 1
                commit_count += 1
                if on_commit is not None:
                    on_commit(tile_id, previous_chain)
                if crash_after_commits is not None and commit_count >= crash_after_commits:
                    trace.emit(
                        "simulated_forced_termination",
                        committed_during_run=commit_count,
                        next_seq=next_seq,
                        chain=previous_chain,
                    )
                    os._exit(75)

            try:
                scheduler.run(missing, task, commit)
            except TileForgeError as exc:
                if exc.code != E_BUDGET_EXCEEDED:
                    trace.emit("run_failed_recovery_retained", recovered_count=len(recovered), committed=commit_count)
                    raise
                journal.discard()
                trace.emit(
                    "run_rejected_no_publish",
                    error_code="E_BUDGET_EXCEEDED",
                    previous_version_preserved=True,
                )
                raise
            except BaseException:
                trace.emit("run_failed_recovery_retained", recovered_count=len(recovered), committed=commit_count)
                raise
        else:
            trace.emit("scheduler_skipped_all_tiles_reused", affected_count=0)

        output_sequence = _build_output_sequence(
            input_sequence,
            config,
            outputs,
            base.sequence if base is not None else None,
            set(reused_tiles),
        )
        records: list[dict[str, Any]] = []
        max_code_deltas: dict[int, int] = {}
        for tile_id in input_sequence.tile_ids():
            block = output_sequence.block_map()[tile_id]
            if tile_id in reused_tiles:
                old = old_records.get(tile_id, {})
                max_code_deltas[tile_id] = int(old.get("max_code_delta", 0))
                records.append(
                    _tile_record(
                        block,
                        fingerprints[tile_id],
                        input_blocks[tile_id].content_hash.hex(),
                        int(old.get("max_code_delta", 0)),
                        str(old.get("profile", "reused")),
                        old.get("origin_version"),
                        True,
                        params_version,
                    )
                )
                downgrade_stages[tile_id] = str(old.get("profile", "reused"))
            else:
                meta = result_meta[tile_id]
                max_code_deltas[tile_id] = int(meta["max_code_delta"])
                records.append(
                    _tile_record(
                        block,
                        fingerprints[tile_id],
                        input_blocks[tile_id].content_hash.hex(),
                        int(meta["max_code_delta"]),
                        str(meta["profile"]),
                        None,
                        False,
                        params_version,
                    )
                )
        manifest = store.publish_sequence(
            output_sequence,
            config=config.to_dict(),
            config_hash=config.config_hash(),
            conversion_id=config.conversion_id(),
            input_hash=input_hash,
            sync_marker=config.observation.sync_marker,
            observation=config.observation.to_dict(),
            affected_tiles=affected_tiles,
            reused_tiles=reused_tiles,
            downgrade_stages=downgrade_stages,
            tile_records=records,
            parameter_version=params_version,
            base_manifest=base.manifest if base is not None else None,
        )
        journal.discard()
        summary = ConversionSummary(
            version_id=int(manifest["version_id"]),
            version_hash=str(manifest["version_hash"]),
            affected_tiles=affected_tiles,
            reused_tiles=reused_tiles,
            recomputed_tiles=list(affected_tiles),
            downgrade_stages=downgrade_stages,
            content_hashes={tile_id: output_sequence.block_map()[tile_id].content_hash.hex() for tile_id in input_sequence.tile_ids()},
            max_code_deltas=max_code_deltas,
            trace_path=str(trace_file),
        )
        trace.emit(
            "version_published",
            version_id=summary.version_id,
            version_hash=summary.version_hash,
            affected_count=len(affected_tiles),
            reused_count=len(reused_tiles),
            recomputed_count=len(affected_tiles),
            parameter_version=params_version,
            downgrade_stages={str(key): value for key, value in sorted(downgrade_stages.items())},
            content_hashes=summary.content_hashes,
            max_code_deltas={str(key): value for key, value in sorted(summary.max_code_deltas.items())},
        )
        return summary


def offline_convert(
    input_path: str | Path,
    output_path: str | Path,
    config: JobConfig,
    *,
    trace_path: str | Path | None = None,
) -> ConversionSummary:
    source = Path(input_path)
    destination = Path(output_path)
    input_sequence = parse_sequence(source)
    config.validate_for_sequence(
        input_sequence.header.width,
        input_sequence.header.height,
        input_sequence.header.block_count,
        input_sequence.header.source_transfer,
        input_sequence.header.conversion_id,
    )
    output_hash_identity = hashlib.sha256(
        _canonical(
            {
                "input_hash": input_sequence.file_sha256,
                "config_hash": config.config_hash(),
            }
        ).encode("utf-8")
    ).hexdigest()[:32]
    lock = RunLock(destination.parent / f".tileforge-offline-{output_hash_identity}.lock", output_hash_identity)
    trace_file = Path(trace_path) if trace_path is not None else destination.with_suffix(destination.suffix + ".trace.jsonl")
    trace = TraceLog(trace_file)
    parameter_version = int(config.config_hash()[:8], 16) & 0x7FFFFFFF
    with lock:
        trace.emit(
            "offline_start",
            input_hash=input_sequence.file_sha256,
            config_hash=config.config_hash(),
            sync_marker=config.observation.sync_marker,
            parameter_version=parameter_version,
            observation=config.observation.to_dict(),
        )
        pipeline = ColorPipeline.from_config(config)
        input_blocks = input_sequence.block_map()
        outputs: dict[int, TileBlock] = {}
        result_meta: dict[int, dict[str, Any]] = {}
        scheduler = ConversionScheduler(input_sequence.header, config.memory, trace)

        def task(tile_id: int, profile: str) -> tuple[TileResult, dict[str, Any]]:
            result = convert_tile(input_sequence, input_blocks[tile_id], config, pipeline, profile=profile)
            usage = estimate_tile_memory(
                input_sequence.header,
                profile,
                config.memory.estimator_overhead_bytes,
            ).to_dict()
            return result, usage

        def commit(tile_id: int, value: tuple[TileResult, dict[str, Any]]) -> None:
            result, usage = value
            block = input_blocks[tile_id]
            outputs[tile_id] = make_block(
                tile_id,
                block.x,
                block.y,
                block.width,
                block.height,
                result.exposure,
                input_sequence.header.channels,
                result.data,
            )
            result_meta[tile_id] = {
                "max_code_delta": result.max_code_delta,
                "profile": result.profile,
                "usage_bytes": usage,
                "tone_map_curve": result.tone_map_curve,
            }

        scheduler.run(input_sequence.tile_ids(), task, commit)
        output_sequence = _build_output_sequence(input_sequence, config, outputs, None, set())
        version_hash = write_sequence_atomic(destination, output_sequence)
        records: list[dict[str, Any]] = []
        for block in input_sequence.ordered_blocks():
            meta = result_meta[block.tile_id]
            records.append(
                _tile_record(
                    output_sequence.block_map()[block.tile_id],
                    tile_fingerprint(input_sequence, block, config),
                    block.content_hash.hex(),
                    int(meta["max_code_delta"]),
                    str(meta["profile"]),
                    None,
                    False,
                    parameter_version,
                )
            )
        report = {
            "schema": 1,
            "kind": "tileforge-offline-report",
            "ok": True,
            "input_hash": input_sequence.file_sha256,
            "output_hash": version_hash,
            "config_hash": config.config_hash(),
            "conversion_id": config.conversion_id(),
            "parameter_version": parameter_version,
            "sync_marker": config.observation.sync_marker,
            "observation": config.observation.to_dict(),
            "bit_depth": input_sequence.header.bit_depth,
            "tiles": _offline_tiles(output_sequence, records),
        }
        report_path = destination.with_suffix(destination.suffix + ".report.json")
        atomic_write_bytes(
            report_path,
            json.dumps(report, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n",
        )
        trace.emit(
            "offline_complete",
            output_hash=version_hash,
            output_path=str(destination),
            report_path=str(report_path),
            tile_hashes={str(record["tile_id"]): record["content_hash"] for record in records},
            max_code_deltas={str(record["tile_id"]): record["max_code_delta"] for record in records},
        )
        return ConversionSummary(
            version_id=None,
            version_hash=version_hash,
            affected_tiles=input_sequence.tile_ids(),
            reused_tiles=[],
            recomputed_tiles=input_sequence.tile_ids(),
            downgrade_stages={tile_id: str(meta["profile"]) for tile_id, meta in result_meta.items()},
            content_hashes={
                tile_id: output_sequence.block_map()[tile_id].content_hash.hex()
                for tile_id in input_sequence.tile_ids()
            },
            max_code_deltas={
                tile_id: int(result_meta[tile_id]["max_code_delta"])
                for tile_id in input_sequence.tile_ids()
            },
            trace_path=str(trace_file),
            output_path=str(destination),
        )
