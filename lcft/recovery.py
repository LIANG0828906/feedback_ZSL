"""Resume a layout from a validated on-disk checkpoint."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from typing import Any

from .canonical import atomic_write_json
from .engine import layout_track
from .loader import LoadedBundle
from .params import LayoutParams
from .precompute import precompute_track
from .snapshot import SnapshotStore


def resume_from_snapshot(
    loaded: LoadedBundle,
    snapshot_dir: str | Path,
    language: str,
    out_dir: str | Path,
    *,
    workers: int = 2,
    strict: bool = True,
) -> dict[str, Any]:
    store = SnapshotStore(snapshot_dir, f"speech-{language}")
    recovery = store.load_latest(strict=strict)
    base = recovery.report
    if not recovery.checkpoint:
        destination = Path(out_dir)
        destination.mkdir(parents=True, exist_ok=True)
        atomic_write_json(destination / "resumed-report.json", base)
        summary = {
            "ok": True,
            "checkpoint": False,
            "recovered_from_published_version": recovery.recovered_from_published_version,
            "track_hash": base.get("track_hash"),
            "reused_count": len(base.get("sentences", [])),
            "precomputed_count": 0,
        }
        atomic_write_json(destination / "resume-summary.json", summary)
        return summary

    speech = loaded.normalized.tracks[language]
    descriptions = (
        loaded.normalized.descriptions
        if language == loaded.languages[0]
        else tuple()
    )
    params = LayoutParams.from_dict(recovery.report.get("params", {}))
    completed_rows = recovery.precomputed_sentences
    completed_by_id = {row["id"]: row for row in completed_rows}
    expected_ids = [item.id for item in speech]
    missing_items = tuple(item for item in speech if item.id not in completed_by_id)
    pending_from_disk = list(recovery.pending_sentence_ids)
    missing_ids = [item.id for item in missing_items]
    if pending_from_disk != missing_ids:
        from .errors import LCFTError

        raise LCFTError(
            "E_RECOVERY_INVALID",
            "checkpoint pending sentence list does not match completed rows",
            field="snapshot.pending_sentence_ids",
            details={
                "pending_from_disk": pending_from_disk,
                "derived_missing_ids": missing_ids,
            },
        )
    completed_desc_by_id = {
        row["id"]: row for row in recovery.precomputed_descriptions
    }
    missing_desc = tuple(
        item for item in descriptions if item.id not in completed_desc_by_id
    )
    extra = None
    if missing_items or missing_desc:
        extra = precompute_track(
            f"speech-{language}-resume",
            missing_items,
            missing_desc,
            params,
            loaded.normalized.audio.sample_rate,
            workers=workers,
        )
    new_speech_rows = {row["id"]: row for row in (extra.sentences if extra else [])}
    new_desc_rows = {
        row["id"]: row for row in (extra.descriptions if extra else [])
    }
    ordered_speech = [
        completed_by_id.get(item.id) or new_speech_rows[item.id] for item in speech
    ]
    ordered_desc = [
        completed_desc_by_id.get(item.id) or new_desc_rows[item.id]
        for item in descriptions
    ]
    report = layout_track(
        f"speech-{language}",
        language,
        speech,
        descriptions,
        loaded.normalized,
        params,
        published_version=int(recovery.recovered_from_published_version) + 1,
        param_version=int(recovery.report.get("param_version", 1)),
        precomputed_sentences=ordered_speech,
        precomputed_descriptions=ordered_desc,
    )
    destination = Path(out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    recovery_store = SnapshotStore(destination / "snapshots" / language, f"speech-{language}")
    recovery_store.save_published(report)
    atomic_write_json(destination / "resumed-report.json", report)
    summary = {
        "ok": True,
        "checkpoint": True,
        "recovered_from_published_version": recovery.recovered_from_published_version,
        "snapshot_version": recovery.snapshot_version,
        "track_hash": report["track_hash"],
        "reused_count": len(completed_by_id) + len(completed_desc_by_id),
        "recomputed_count": len(missing_items) + len(missing_desc),
        "recomputed_sentence_ids": missing_ids,
        "recomputed_description_ids": [item.id for item in missing_desc],
        "exit_report_from_killed_process": False,
    }
    atomic_write_json(destination / "resume-summary.json", summary)
    return summary

