from __future__ import annotations

import copy
import threading
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

from .errors import BusinessError, CancelledError, E_BUSY, E_INVALID_INPUT
from .hashing import content_hash
from .layout import (
    Checkpoint,
    ZERO_HASH,
    compare_layouts,
    compute_affected_sentence_ids,
    compute_layout,
)
from .lock import AssetSessionLock
from .models import (
    DescriptionInput,
    LayoutParams,
    SentenceInput,
    SubtitleAsset,
)
from .persistence import (
    STATE_FILE,
    build_state,
    load_state,
    save_json_atomic,
    save_state,
    verify_state,
)
from .timebase import (
    WavInfo,
    load_media,
    format_timecode,
    samples_to_ticks,
    validate_media_and_asset,
)
from .trace import TraceWriter


FaultHook = Callable[[Checkpoint], None]


class LayoutSession:
    """A single-writer layout session with copy-on-write publication."""

    def __init__(
        self,
        *,
        case_dir: Path,
        asset_path: Path,
        wav_path: Path,
        frames_path: Path,
        state_path: Path,
        trace: TraceWriter,
        asset: SubtitleAsset,
        wav: WavInfo,
        frames: Any,
        report: dict[str, Any] | None,
        history: list[dict[str, Any]],
        pending: Mapping[str, Any] | None,
        lock: AssetSessionLock,
    ) -> None:
        self.case_dir = case_dir
        self.asset_path = asset_path
        self.wav_path = wav_path
        self.frames_path = frames_path
        self.state_path = state_path
        self.trace = trace
        self._wav = wav
        self._frames = frames
        self._asset = asset
        self._params = asset.parameters
        self._report = report
        self._history = history
        self._lock = lock
        self._snapshot_lock = threading.RLock()
        self._commit_lock = threading.Lock()
        self._preview_seq = 0
        self._closed = False
        self._cancel_event = threading.Event()
        self._pending_params: LayoutParams | None = None
        self._pending_sentence_updates: dict[str, dict[str, Any]] = {}
        self._pending_descriptions: tuple[DescriptionInput, ...] | None = None
        self._pending_completed: list[str] = []
        self._pending_computed: dict[str, dict[str, Any]] = {}
        self._pending_from_disk = pending is not None
        if pending is not None:
            self._restore_pending(pending)
            self.trace.write(
                "recovery",
                asset_id=asset.asset_id,
                commit_hash=report["commit_hash"] if report else None,
                revision=report["revision"] if report else 0,
                pending_sentence_ids=list(pending["affected_sentence_ids"]),
                completed_sentence_ids=list(pending["completed_sentence_ids"]),
            )

    @classmethod
    def open(
        cls,
        case_dir: Path | str,
        *,
        asset_path: Path | str | None = None,
        wav_path: Path | str | None = None,
        frames_path: Path | str | None = None,
        state_path: Path | str | None = None,
        trace_path: Path | str | None = None,
        truncate_trace: bool = False,
    ) -> "LayoutSession":
        case_dir = Path(case_dir)
        asset_path = Path(asset_path) if asset_path else case_dir / "subtitles.json"
        wav_path = Path(wav_path) if wav_path else case_dir / "program.wav"
        frames_path = Path(frames_path) if frames_path else case_dir / "frames.ftbl"
        state_path = Path(state_path) if state_path else case_dir / STATE_FILE
        trace_path = Path(trace_path) if trace_path else case_dir / "trace.jsonl"
        lock = AssetSessionLock(state_path.with_name(state_path.name + ".lock"))
        lock.acquire()
        trace = TraceWriter(trace_path, truncate=truncate_trace)
        try:
            source_asset = SubtitleAsset.load(asset_path)
            wav = load_media(source_asset, wav_path, frames_path)[0]
            from .timebase import read_frame_table

            frames = read_frame_table(frames_path)
            state = load_state(state_path)
            if state is None:
                validate_media_and_asset(source_asset, wav, frames, source_asset.parameters)
                return cls(
                    case_dir=case_dir,
                    asset_path=asset_path,
                    wav_path=wav_path,
                    frames_path=frames_path,
                    state_path=state_path,
                    trace=trace,
                    asset=source_asset,
                    wav=wav,
                    frames=frames,
                    report=None,
                    history=[],
                    pending=None,
                    lock=lock,
                )
            asset = SubtitleAsset.from_dict(state["inputs"])
            params = LayoutParams.from_dict(state["params"])
            asset = asset.replace(parameters=params)
            validate_media_and_asset(asset, wav, frames, params)
            return cls(
                case_dir=case_dir,
                asset_path=asset_path,
                wav_path=wav_path,
                frames_path=frames_path,
                state_path=state_path,
                trace=trace,
                asset=asset,
                wav=wav,
                frames=frames,
                report=dict(state["report"]),
                history=[dict(item) for item in state["history"]],
                pending=state["pending"],
                lock=lock,
            )
        except BaseException:
            trace.close()
            lock.release()
            raise

    def _restore_pending(self, pending: Mapping[str, Any]) -> None:
        self._pending_params = LayoutParams.from_dict(pending["params"])
        updates = pending["sentence_updates"]
        if not isinstance(updates, list):
            raise BusinessError("E_RECOVERY_INVALID", "pending sentence updates are invalid")
        for item in updates:
            sentence_id = item.get("id")
            changes = item.get("changes")
            if not isinstance(sentence_id, str) or not isinstance(changes, Mapping):
                raise BusinessError("E_RECOVERY_INVALID", "pending sentence update is invalid")
            self._pending_sentence_updates[sentence_id] = dict(changes)
        descriptions = pending.get("descriptions")
        if descriptions is not None:
            if not isinstance(descriptions, list):
                raise BusinessError("E_RECOVERY_INVALID", "pending descriptions are invalid")
            self._pending_descriptions = tuple(
                DescriptionInput.from_dict(item, self._params.default_cps) for item in descriptions
            )
        completed = pending["completed_sentence_ids"]
        computed = pending["computed_entries"]
        self._pending_completed = list(completed)
        self._pending_computed = {str(key): copy.deepcopy(dict(value)) for key, value in computed.items()}

    @property
    def report(self) -> dict[str, Any] | None:
        with self._snapshot_lock:
            return self._report

    @property
    def revision(self) -> int:
        report = self.report
        return int(report["revision"]) if report else 0

    @property
    def commit_hash(self) -> str | None:
        report = self.report
        return str(report["commit_hash"]) if report else None

    @property
    def params(self) -> LayoutParams:
        return self._params

    @property
    def asset(self) -> SubtitleAsset:
        return self._asset

    @property
    def has_pending(self) -> bool:
        return bool(
            self._pending_params is not None
            or self._pending_sentence_updates
            or self._pending_descriptions is not None
            or self._pending_completed
        )

    def pending_info(self) -> dict[str, Any]:
        affected = self._candidate_affected()
        unfinished = [item for item in affected if item not in set(self._pending_completed)]
        return {
            "affected_sentence_ids": affected,
            "completed_sentence_ids": sorted(set(self._pending_completed)),
            "unfinished_sentence_ids": sorted(set(unfinished)),
            "recovered_from_disk": self._pending_from_disk,
        }

    def stage_params(self, updates: Mapping[str, Any]) -> None:
        self._ensure_open()
        self._ensure_not_committing()
        self._invalidate_recovery()
        base = self._pending_params or self._params
        self._pending_params = base.with_updates(updates)

    def stage_sentence(self, sentence_id: str, updates: Mapping[str, Any]) -> None:
        self._ensure_open()
        self._ensure_not_committing()
        self.asset.sentence_by_id(sentence_id)
        self._invalidate_recovery()
        current = dict(self._pending_sentence_updates.get(sentence_id, {}))
        current.update(updates)
        # Validate against the committed sentence now, so the UI gets an early
        # error instead of failing after a long commit.
        self.asset.sentence_by_id(sentence_id).with_updates(current)
        self._pending_sentence_updates[sentence_id] = current

    def stage_descriptions(self, descriptions: Sequence[Mapping[str, Any] | DescriptionInput]) -> None:
        self._ensure_open()
        self._ensure_not_committing()
        self._invalidate_recovery()
        values: list[DescriptionInput] = []
        base = self._pending_params or self._params
        for item in descriptions:
            if isinstance(item, DescriptionInput):
                values.append(item)
            else:
                values.append(DescriptionInput.from_dict(item, base.default_cps))
        self._pending_descriptions = tuple(values)

    def _invalidate_recovery(self) -> None:
        if self._pending_completed:
            self._pending_completed.clear()
            self._pending_computed.clear()
            self.trace.write(
                "recovery_invalidated",
                asset_id=self.asset.asset_id,
                reason="new edits were staged",
            )

    def _candidate_asset(self) -> SubtitleAsset:
        sentences: list[SentenceInput] = []
        for sentence in self._asset.sentences:
            changes = self._pending_sentence_updates.get(sentence.id)
            sentences.append(sentence.with_updates(changes) if changes else sentence)
        descriptions = (
            self._pending_descriptions
            if self._pending_descriptions is not None
            else self._asset.descriptions
        )
        params = self._pending_params or self._params
        return SubtitleAsset(
            asset_id=self._asset.asset_id,
            sentences=tuple(sentences),
            descriptions=tuple(descriptions),
            parameters=params,
        )

    def _candidate_affected(self) -> list[str]:
        candidate = self._candidate_asset()
        if self._report is None:
            return sorted(item.id for item in candidate.sentences)
        return compute_affected_sentence_ids(
            old_asset=self._asset,
            new_asset=candidate,
            old_params=self._params,
            new_params=candidate.parameters,
            previous_report=self._report,
        )

    def _pending_record(
        self,
        *,
        candidate: SubtitleAsset,
        affected_ids: Sequence[str],
        completed: Sequence[str],
        computed: Mapping[str, Mapping[str, Any]],
    ) -> dict[str, Any]:
        return {
            "affected_sentence_ids": sorted(set(affected_ids)),
            "base_revision": self.revision,
            "completed_sentence_ids": sorted(set(completed)),
            "computed_entries": {key: copy.deepcopy(dict(value)) for key, value in sorted(computed.items())},
            "descriptions": (
                [item.to_dict() for item in candidate.descriptions]
                if self._pending_descriptions is not None
                else None
            ),
            "operation_hash": content_hash(candidate.to_dict()),
            "params": candidate.parameters.to_dict(),
            "sentence_updates": [
                {"changes": copy.deepcopy(changes), "id": sentence_id}
                for sentence_id, changes in sorted(self._pending_sentence_updates.items())
            ],
        }

    def _persist_committed(self, pending: Mapping[str, Any] | None) -> None:
        if self._report is None:
            # There is no committed report to anchor a recoverable journal yet.
            if pending is not None:
                return
            return
        state = build_state(
            asset=self._asset,
            params=self._params,
            report=self._report,
            history=self._history,
            pending=pending,
        )
        verify_state(state)
        save_state(self.state_path, state)

    def request_cancel(self) -> None:
        self._cancel_event.set()

    def _cancel_requested(self) -> bool:
        return self._cancel_event.is_set()

    def commit(
        self,
        *,
        fault_hook: FaultHook | None = None,
    ) -> dict[str, Any]:
        self._ensure_open()
        if not self._commit_lock.acquire(blocking=False):
            raise BusinessError("E_BUSY", "a commit is already running for this session")
        try:
            self._cancel_event.clear()
            candidate = self._candidate_asset()
            if self._report is not None and not self.has_pending:
                return self._report
            timings = validate_media_and_asset(candidate, self._wav, self._frames, candidate.parameters)
            del timings
            affected = self._candidate_affected()
            recovered_entries = self._pending_computed if self.has_pending else {}
            revision = self.revision + 1
            parent_hash = self.commit_hash or ZERO_HASH
            completed: list[str] = list(self._pending_completed)
            computed: dict[str, dict[str, Any]] = {
                key: copy.deepcopy(value) for key, value in self._pending_computed.items()
            }
            pending_record = self._pending_record(
                candidate=candidate,
                affected_ids=affected,
                completed=completed,
                computed=computed,
            )
            self._persist_committed(pending_record)
            self.trace.write(
                "journal",
                asset_id=candidate.asset_id,
                affected_sentence_ids=sorted(affected),
                pending_sentence_ids=sorted(
                    item for item in affected if item not in set(completed)
                ),
                revision=self.revision,
            )

            def on_checkpoint(point: Checkpoint) -> None:
                if point.sentence_id not in set(affected):
                    return
                if point.sentence_id not in completed:
                    completed.append(point.sentence_id)
                computed[point.sentence_id] = copy.deepcopy(dict(point.entry))
                record = self._pending_record(
                    candidate=candidate,
                    affected_ids=affected,
                    completed=completed,
                    computed=computed,
                )
                self._persist_committed(record)
                self.trace.write(
                    "checkpoint",
                    asset_id=candidate.asset_id,
                    checkpoint_sentence_id=point.sentence_id,
                    completed_count=len(completed),
                    revision=self.revision,
                )
                if fault_hook is not None:
                    fault_hook(point)

            report = compute_layout(
                candidate,
                self._wav,
                self._frames,
                params=candidate.parameters,
                revision=revision,
                parent_hash=parent_hash,
                previous_report=self._report,
                affected_ids=affected,
                recovered_entries=recovered_entries,
                checkpoint=on_checkpoint,
                cancel_requested=self._cancel_requested,
            )
            history = list(self._history)
            history.append(
                {
                    "commit_hash": report["commit_hash"],
                    "parent_hash": report["parent_hash"],
                    "revision": report["revision"],
                }
            )
            final_state = build_state(
                asset=candidate,
                params=candidate.parameters,
                report=report,
                history=history,
                pending=None,
            )
            verify_state(final_state)
            save_state(self.state_path, final_state)
            with self._snapshot_lock:
                self._asset = candidate
                self._params = candidate.parameters
                self._report = report
                self._history = history
            self._clear_pending()
            self.trace.write(
                "commit",
                affected=report["counts"]["affected"],
                affected_sentence_ids=report["affected_sentence_ids"],
                asset_id=report["asset_id"],
                commit_hash=report["commit_hash"],
                recomputed=report["counts"]["recomputed"],
                recovered=report["counts"]["recovered"],
                reused=report["counts"]["reused"],
                reused_sentence_ids=report["reused_sentence_ids"],
                revision=report["revision"],
            )
            return report
        except CancelledError as exc:
            self._rollback_pending()
            self.trace.write(
                "cancel",
                asset_id=self.asset.asset_id,
                checkpoint_context=dict(exc.context),
                revision=self.revision,
            )
            raise
        except BaseException:
            self._rollback_pending()
            self.trace.write(
                "error",
                asset_id=self.asset.asset_id,
                revision=self.revision,
            )
            raise
        finally:
            self._commit_lock.release()

    def _rollback_pending(self) -> None:
        self._pending_completed.clear()
        self._pending_computed.clear()
        if self._report is not None:
            self._persist_committed(None)

    def _clear_pending(self) -> None:
        self._pending_params = None
        self._pending_sentence_updates.clear()
        self._pending_descriptions = None
        self._pending_completed.clear()
        self._pending_computed.clear()
        self._pending_from_disk = False

    def discard_pending(self) -> None:
        self._ensure_open()
        self._ensure_not_committing()
        self._clear_pending()
        if self._report is not None:
            self._persist_committed(None)
        self.trace.write("pending_discarded", asset_id=self.asset.asset_id, revision=self.revision)

    def refresh(self, position_sample: int, *, window_samples: int = 240_000) -> dict[str, Any]:
        self._ensure_open()
        if position_sample < 0 or window_samples <= 0:
            raise BusinessError(
                E_INVALID_INPUT,
                "preview position and window must be non-negative",
                {"position_sample": position_sample, "window_samples": window_samples},
            )
        with self._snapshot_lock:
            report = self._report
            if report is None:
                raise BusinessError(E_INVALID_INPUT, "no committed layout is available")
            self._preview_seq += 1
            sequence = self._preview_seq
            window_start = position_sample
            window_end = position_sample + window_samples
            selected = [
                copy.deepcopy(item)
                for item in report["sentences"]
                if int(item["end_sample"]) > window_start and int(item["start_sample"]) < window_end
            ]
            payload = {
                "asset_id": report["asset_id"],
                "commit_hash": report["commit_hash"],
                "position_sample": position_sample,
                "refresh_seq": sequence,
                "revision": report["revision"],
                "sentence_ids": [item["sentence_id"] for item in selected],
                "sentences": selected,
                "window": [window_start, window_end],
                "window_timecodes": [
                    format_timecode(samples_to_ticks(window_start)),
                    format_timecode(samples_to_ticks(window_end)),
                ],
            }
        self.trace.write(
            "refresh",
            affected=report["counts"]["affected"],
            asset_id=report["asset_id"],
            commit_hash=report["commit_hash"],
            position_sample=position_sample,
            refresh_seq=sequence,
            reused=report["counts"]["reused"],
            revision=report["revision"],
            sentence_hashes={item["sentence_id"]: item["entry_hash"] for item in selected},
            sentence_ids=payload["sentence_ids"],
            timecodes={item["sentence_id"]: [item["start"], item["end"]] for item in selected},
            window=payload["window"],
            window_timecodes=payload["window_timecodes"],
        )
        return payload

    def offline_layout(self, *, asset: SubtitleAsset | None = None) -> dict[str, Any]:
        self._ensure_open()
        target = asset or self._asset
        report = compute_layout(
            target,
            self._wav,
            self._frames,
            params=target.parameters,
            revision=1,
            parent_hash=ZERO_HASH,
            previous_report=None,
        )
        self.trace.write(
            "offline",
            asset_id=report["asset_id"],
            commit_hash=report["commit_hash"],
            sentence_hashes={
                item["sentence_id"]: item["entry_hash"] for item in report["sentences"]
            },
            sentence_ids=[item["sentence_id"] for item in report["sentences"]],
        )
        return report

    def compare_with_offline(self, *, sync_markers: Sequence[str] | None = None) -> dict[str, Any]:
        if self._report is None:
            raise BusinessError(E_INVALID_INPUT, "no committed layout is available")
        offline = self.offline_layout()
        online = self._report
        try:
            compare_layouts(online, offline, sync_markers=sync_markers)
        except BusinessError as exc:
            self.trace.write(
                "mismatch",
                asset_id=online["asset_id"],
                online_commit_hash=online["commit_hash"],
                offline_commit_hash=offline["commit_hash"],
                context=dict(exc.context),
            )
            raise
        markers = (
            list(sync_markers)
            if sync_markers is not None
            else [item["sentence_id"] for item in online["sentences"]]
        )
        self.trace.write(
            "offline_equal",
            asset_id=online["asset_id"],
            offline_commit_hash=offline["commit_hash"],
            online_commit_hash=online["commit_hash"],
            sync_markers=markers,
        )
        return offline

    def write_report(self, path: Path | str, report: Mapping[str, Any] | None = None) -> None:
        self._ensure_open()
        value = report or self._report
        if value is None:
            raise BusinessError(E_INVALID_INPUT, "no report to write")
        save_json_atomic(path, value)

    def close(self, *, discard_pending: bool = False) -> None:
        if self._closed:
            return
        try:
            if discard_pending and self.has_pending:
                self._rollback_pending()
        finally:
            self._closed = True
            self.trace.close()
            self._lock.release()

    def _ensure_open(self) -> None:
        if self._closed:
            raise BusinessError(E_INVALID_INPUT, "layout session is closed")

    def _ensure_not_committing(self) -> None:
        if self._commit_lock.locked():
            raise BusinessError(
                E_BUSY,
                "edits cannot be staged while a commit is running",
                {"asset_id": self.asset.asset_id},
            )

    def __enter__(self) -> "LayoutSession":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()

