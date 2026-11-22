"""Deterministic batch orchestration, parallel precompute and publication."""

from __future__ import annotations

import threading
from copy import deepcopy
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .budget import BudgetController, Usage, select_degradation
from .canonical import atomic_write_json
from .engine import layout_track
from .errors import LCFTError
from .loader import LoadedBundle, load_bundle
from .params import LayoutParams
from .precompute import (
    GLOBAL_TASK_REGISTRY,
    PrecomputeResult,
    TaskRegistry,
    precompute_track,
)
from .snapshot import SnapshotStore
from .version_store import VersionStore


@dataclass(frozen=True)
class BatchOptions:
    workers: int = 2
    global_quota: int = 256 * 1024 * 1024
    job_quota: int = 128 * 1024 * 1024
    description_track: str | None = None
    write_snapshots: bool = True
    cancel_event: threading.Event | None = None
    task_registry: TaskRegistry = field(default_factory=lambda: GLOBAL_TASK_REGISTRY)


class TraceLog:
    def __init__(self) -> None:
        self.refresh = 0
        self.events: list[dict[str, Any]] = []

    def emit(self, event: str, **values: Any) -> dict[str, Any]:
        self.refresh += 1
        row = {"event": event, "refresh": self.refresh, **values}
        self.events.append(row)
        return row


def _text_bytes(items: tuple[Any, ...]) -> int:
    return sum(len(item.text.encode("utf-8")) for item in items)


def _job_id(language: str, track_id: str) -> str:
    return f"job-{language}"


def run_batch(
    bundle: LoadedBundle | str | Path,
    out_dir: str | Path,
    params: LayoutParams | dict[str, Any] | None = None,
    options: BatchOptions | None = None,
) -> dict[str, Any]:
    """Run all language tracks; publish each track only after a complete layout."""

    loaded = bundle if isinstance(bundle, LoadedBundle) else load_bundle(bundle)
    layout_params = params if isinstance(params, LayoutParams) else LayoutParams.from_dict(params)
    layout_params.validate()
    opts = options or BatchOptions()
    if opts.workers < 1:
        raise ValueError("workers must be at least 1")
    if opts.global_quota <= 0 or opts.job_quota <= 0:
        raise ValueError("budget quotas must be positive")

    destination = Path(out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    (destination / "tracks").mkdir(exist_ok=True)
    trace = TraceLog()
    summary: dict[str, Any] = {
        "schema_version": 1,
        "ok": True,
        "languages": loaded.languages,
        "published_hashes": {},
        "track_files": {},
        "budget_ledger": [],
        "budget_rejections": [],
        "scheduler_trace": trace.events,
        "audio_channels": loaded.normalized.audio.channels,
        "sample_rate": loaded.normalized.audio.sample_rate,
        "frame_rate": loaded.normalized.frames.to_dict()["frame_rate"],
        "seed": loaded.raw.get("seed"),
        "shape": loaded.raw.get("shape"),
        "descriptions": len(loaded.normalized.descriptions),
    }
    description_track = opts.description_track
    if description_track is None and loaded.languages:
        description_track = loaded.languages[0]
    if description_track is not None and description_track not in loaded.normalized.tracks:
        raise LCFTError(
            "E_INPUT_INVALID",
            "description host track is not present",
            field="description_track",
            details={"description_track": description_track},
        )

    budget = BudgetController(opts.global_quota)
    stores: dict[str, VersionStore] = {}

    for language in loaded.languages:
        track_id = f"speech-{language}"
        job = _job_id(language, track_id)
        speech = loaded.normalized.tracks[language]
        descriptions = (
            loaded.normalized.descriptions
            if language == description_track
            else tuple()
        )
        text_bytes = _text_bytes(speech) + _text_bytes(descriptions)
        sentence_count = len(speech) + len(descriptions)
        try:
            opts.task_registry.acquire(track_id, job)
        except LCFTError as exc:
            trace.emit(
                "busy",
                job=job,
                track=track_id,
                code=exc.code,
                field=exc.field,
            )
            summary["ok"] = False
            summary["error"] = exc.to_dict()
            break

        usage: Usage | None = None
        try:
            try:
                usage = select_degradation(
                    sentence_count=sentence_count,
                    text_bytes=text_bytes,
                    job_quota=opts.job_quota,
                    global_quota=opts.global_quota,
                )
            except LCFTError as exc:
                trace.emit(
                    "budget_rejected",
                    job=job,
                    track=track_id,
                    stage="none",
                    usage_bytes=0,
                    code=exc.code,
                    field=exc.field,
                    attempts=exc.details.get("attempts", []),
                )
                summary["budget_rejections"].append(
                    {
                        "job": job,
                        "track": track_id,
                        "code": exc.code,
                        "field": exc.field,
                        "attempts": exc.details.get("attempts", []),
                    }
                )
                summary["ok"] = False
                summary["error"] = exc.to_dict()
                summary["published_unchanged_after_rejection"] = True
                break

            assert usage is not None
            trace.emit(
                "budget_stage",
                job=job,
                track=track_id,
                stage=usage.stage,
                stage_index=usage.stage_index,
                usage_bytes=usage.total_bytes,
                attempts=getattr(usage, "attempts", []),
            )
            budget.acquire(job, usage)
            trace.emit(
                "admitted",
                job=job,
                track=track_id,
                stage=usage.stage,
                stage_index=usage.stage_index,
                usage_bytes=usage.total_bytes,
                queued=any(
                    entry.get("event") == "queued" and entry.get("job") == job
                    for entry in budget.ledger
                ),
            )
            if opts.cancel_event is not None and opts.cancel_event.is_set():
                raise LCFTError(
                    "E_CANCELLED",
                    "batch was cooperatively cancelled before start",
                    field="cancel_event",
                )
            trace.emit("start", job=job, track=track_id, stage=usage.stage)
            precomputed = precompute_track(
                track_id,
                speech,
                descriptions,
                layout_params,
                loaded.normalized.audio.sample_rate,
                workers=opts.workers,
                cancel_event=opts.cancel_event,
            )
            for decision in precomputed.dispatch_decisions:
                trace.emit(
                    decision["event"],
                    job=job,
                    track=track_id,
                    **{
                        key: value
                        for key, value in decision.items()
                        if key not in ("event", "track")
                    },
                )
            trace.emit(
                "layout_started",
                job=job,
                track=track_id,
                ordered=True,
                sentences=len(speech),
            )
            report = layout_track(
                track_id,
                language,
                speech,
                descriptions,
                loaded.normalized,
                layout_params,
                published_version=1,
                param_version=1,
                precomputed_sentences=precomputed.sentences,
                precomputed_descriptions=precomputed.descriptions,
            )
            trace.emit(
                "layout_committed",
                job=job,
                track=track_id,
                ordered=True,
                sentences=len(report["sentences"]),
            )
            if descriptions:
                trace.emit(
                    "descriptions_committed",
                    job=job,
                    track=track_id,
                    descriptions=len(report["descriptions"]),
                    trims=len(report["trims"]),
                    ordered=True,
                )

            snapshot_store: SnapshotStore | None = None
            if opts.write_snapshots:
                snapshot_store = SnapshotStore(
                    destination / "snapshots" / language, track_id
                )
            store = VersionStore(snapshot_store=snapshot_store)
            store.publish(report)
            report = store.preview() or report
            stores[language] = store
            track_path = destination / "tracks" / f"{language}.json"
            legacy_path = destination / f"sentences-{language}.json"
            atomic_write_json(track_path, report)
            atomic_write_json(legacy_path, report)
            trace.emit(
                "published",
                job=job,
                track=track_id,
                version=report["published_version"],
                track_hash=report["track_hash"],
                stage=usage.stage,
            )
            trace.emit(
                "track_ready",
                job=job,
                track=track_id,
                track_hash=report["track_hash"],
                sentences=len(report["sentences"]),
                descriptions=len(report["descriptions"]),
                affected=len(report["sentences"]) + len(report["descriptions"]),
                reused=0,
                degradation_stage=usage.stage,
                usage_bytes=usage.total_bytes,
            )
            summary["published_hashes"][language] = report["track_hash"]
            summary["track_files"][language] = str(track_path)
        except LCFTError as exc:
            if exc.code == "E_CANCELLED":
                trace.emit(
                    "cancelled",
                    job=job,
                    track=track_id,
                    code=exc.code,
                    field=exc.field,
                    published=False,
                )
                summary["ok"] = False
                summary["cancelled"] = True
                summary["error"] = exc.to_dict()
                break
            if exc.code == "E_BUDGET_EXCEEDED":
                summary["budget_rejections"].append(
                    {
                        "job": job,
                        "track": track_id,
                        "code": exc.code,
                        "field": exc.field,
                        "attempts": exc.details.get("attempts", []),
                    }
                )
            summary["ok"] = False
            summary["error"] = exc.to_dict()
            trace.emit(
                "failed",
                job=job,
                track=track_id,
                code=exc.code,
                field=exc.field,
                published=False,
            )
            break
        finally:
            if usage is not None and budget.used_bytes:
                budget.release(job, usage)
                trace.emit(
                    "released",
                    job=job,
                    track=track_id,
                    stage=usage.stage,
                    usage_bytes=usage.total_bytes,
                )
            opts.task_registry.release(track_id, job)

    summary["budget_ledger"] = deepcopy(budget.ledger)
    if loaded.raw.get("baseline_hashes"):
        summary["baseline_hashes"] = loaded.raw["baseline_hashes"]
        summary["baseline_match"] = {
            language: summary["published_hashes"].get(language)
            == loaded.raw["baseline_hashes"].get(language)
            for language in sorted(loaded.raw["baseline_hashes"])
        }
    summary["workbench_refresh_count"] = trace.refresh
    atomic_write_json(destination / "trace-log.json", trace.events)
    atomic_write_json(destination / "summary.json", summary)
    if not summary["ok"]:
        atomic_write_json(destination / "errors.json", summary["error"])
    return summary

