"""Canonical-model -> target-schema export with lossy downgrade warnings."""

from __future__ import annotations

from typing import Any

from ..constants import SUPPORTED_SCHEMA_VERSIONS
from ..errors import LossyError, SchemaError
from . import v1_loader, v2_loader, v3_loader

_EXPORTERS = {
    "1.0": v1_loader,
    "2.0": v2_loader,
    "3.0": v3_loader,
}


def _warning(field_path: str, event_or_note_id: str, reason: str) -> dict[str, Any]:
    return {
        "field_path": field_path,
        "object_id": event_or_note_id,
        "reason": reason,
    }


def preview_lossy(score, target_version: str) -> list[dict[str, Any]]:
    """Return the E_LOSSY warning list for a downgrade (never raises)."""
    if target_version not in SUPPORTED_SCHEMA_VERSIONS:
        raise SchemaError(
            f"Unknown target schema version {target_version!r}",
            {"version": target_version, "field_path": "target_version"},
        )
    warnings: list[dict[str, Any]] = []
    for note in score.notes:
        note_path = f"score.notes[{note.note_id}]"
        if target_version == "1.0":
            if note.articulation == "accent":
                warnings.append(_warning(
                    f"{note_path}.articulation", note.note_id,
                    "articulation 'accent' is not representable in v1.0; "
                    "coerced to 'tenuto'",
                ))
            if note.expression_mark in ("rit", "a_tempo", "fermata"):
                warnings.append(_warning(
                    f"{note_path}.expression_mark", note.note_id,
                    f"mark {note.expression_mark!r} is not representable in "
                    "v1.0; coerced to 'none'",
                ))
        if target_version in ("1.0", "2.0") and note.interactive_layer is not None:
            warnings.append(_warning(
                f"{note_path}.interactive_layer", note.note_id,
                "interactive layer tag cannot be stored before v3.0; dropped",
            ))
        if target_version == "2.0" and note.expression_mark == "fermata":
            warnings.append(_warning(
                f"{note_path}.expression_mark", note.note_id,
                "mark 'fermata' is not representable in v2.0; coerced to 'none'",
            ))

    for event in score.events:
        event_path = f"score.events[{event.event_id}]"
        if target_version in ("1.0", "2.0") and event.kind == "pitch_bend":
            warnings.append(_warning(
                f"{event_path}.kind", event.event_id,
                "pitch_bend events do not exist before v3.0; event dropped",
            ))
        if target_version == "1.0" and event.kind == "pedal" and event.depth:
            warnings.append(_warning(
                f"{event_path}.depth", event.event_id,
                "fractional pedal depth is not representable in v1.0; "
                "collapsed to boolean on/off",
            ))
        if target_version == "2.0" and event.kind == "pedal" and event.depth:
            warnings.append(_warning(
                f"{event_path}.depth", event.event_id,
                "fractional pedal depth is not representable in v2.0; "
                "collapsed to boolean on/off",
            ))
    return warnings


def _coerce_for_target(score, target_version: str):
    """Return a shallow-adjusted clone honouring the target dialect."""
    clone = score.clone()
    if target_version == "3.0":
        return clone

    kept_events = []
    for event in clone.events:
        if event.kind == "pitch_bend":
            continue
        if event.kind == "pedal":
            event.depth = None
            event.value = 1.0 if event.value >= 0.5 else 0.0
        kept_events.append(event)
    clone.events = kept_events

    for note in clone.notes:
        note.interactive_layer = None
        if target_version == "1.0":
            if note.articulation == "accent":
                note.articulation = "tenuto"
            if note.expression_mark in ("rit", "a_tempo", "fermata"):
                note.expression_mark = "none"
        elif target_version == "2.0":
            if note.expression_mark == "fermata":
                note.expression_mark = "none"
    clone.rebuild_index()
    return clone


def export_score(
    score,
    target_version: str,
    provenance: dict[str, Any] | None = None,
    *,
    confirm_lossy: bool = False,
) -> dict[str, Any]:
    """Export the canonical model.

    A downgrade that would lose information raises :class:`LossyError` unless
    ``confirm_lossy`` is ``True``; the warning list is always available via
    :func:`preview_lossy`.
    """
    warnings = preview_lossy(score, target_version)
    if warnings and not confirm_lossy:
        raise LossyError.from_warnings(warnings)
    adapted = _coerce_for_target(score, target_version)
    module = _EXPORTERS[target_version]
    exported = module.export(adapted, provenance)
    exported["_lossy_warnings"] = warnings
    return exported
