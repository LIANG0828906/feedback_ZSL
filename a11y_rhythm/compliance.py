"""Self-contained per-sentence accessibility compliance evaluation."""

from __future__ import annotations

from typing import Any

from .models import (
    ComplianceReport,
    DescriptionTrack,
    ItemResult,
    SolveResult,
    SourceBundle,
    SpeechTrack,
    content_hash,
    items_to_track,
    samples_to_timecode,
)


def build_compliance_report(
    bundle: SourceBundle,
    result: SolveResult,
) -> ComplianceReport:
    by_id = result.by_id()
    speech = [item for item in result.items if item.kind == "speech"]
    descriptions: list[dict[str, Any]] = []
    violations: list[dict[str, Any]] = []
    source_by_id = {item.item_id: item for item in bundle.items}

    rules = {
        "sample_rate": bundle.sample_rate,
        "speech_description_overlap_allowed": False,
        "description_minimum_gap_rule": "actual_gap_samples >= min_gap_ms converted at sample_rate",
        "description_readability_rule": "display_duration_samples >= ceil(text_length * sample_rate / max_reading_rate_cps)",
        "description_status_rule": "rejected descriptions are non-deliverable",
        "readability_lower_bound_from": "max_reading_rate_cps",
        "evaluation": "all fields required to reconstruct each conclusion are included per statement",
    }

    for desc in [item for item in result.items if item.kind == "description"]:
        source = source_by_id[desc.item_id]
        checks: list[dict[str, Any]] = []
        if desc.status == "rejected":
            violation = {
                "type": "description_rejected",
                "description_id": desc.item_id,
                "conflict_objects": desc.depends_on,
                "description_interval": [desc.start, desc.end],
                "minimum_duration_samples": desc.minimum_duration,
                "actual_duration_samples": desc.duration,
                "required_gap_samples": source.min_gap_samples,
                "reason": "higher-priority speech or description won the duration conflict",
            }
            violations.append(violation)
            checks.append({"check": "status", "passed": False, **violation})
        elif desc.duration < desc.minimum_duration or not desc.readable:
            violation = {
                "type": "description_unreadable",
                "description_id": desc.item_id,
                "conflict_objects": desc.depends_on,
                "description_interval": [desc.start, desc.end],
                "minimum_duration_samples": desc.minimum_duration,
                "actual_duration_samples": desc.duration,
                "required_gap_samples": source.min_gap_samples,
                "text_length": source.text_length,
                "max_reading_rate_cps": source.max_cps,
            }
            violations.append(violation)
            checks.append({"check": "readability", "passed": False, **violation})
        else:
            checks.append(
                {
                    "check": "readability",
                    "passed": True,
                    "description_id": desc.item_id,
                    "minimum_duration_samples": desc.minimum_duration,
                    "actual_duration_samples": desc.duration,
                }
            )

        for speech_item in speech:
            overlap_start = max(desc.start, speech_item.start)
            overlap_end = min(desc.end, speech_item.end)
            overlap = max(0, overlap_end - overlap_start)
            if overlap > 0:
                actual_gap = 0
                passed = False
                check_type = "overflow"
            elif speech_item.end <= desc.start:
                actual_gap = desc.start - speech_item.end
                passed = actual_gap >= source.min_gap_samples
                check_type = "min_gap"
            elif desc.end <= speech_item.start:
                actual_gap = speech_item.start - desc.end
                passed = actual_gap >= source.min_gap_samples
                check_type = "min_gap"
            else:
                actual_gap = 0
                passed = False
                check_type = "unexpected_interval_order"

            pair_check = {
                "check": check_type,
                "passed": passed,
                "description_id": desc.item_id,
                "speech_id": speech_item.item_id,
                "description_interval": [desc.start, desc.end],
                "speech_interval": [speech_item.start, speech_item.end],
                "overlap_samples": overlap,
                "required_gap_samples": source.min_gap_samples,
                "actual_gap_samples": actual_gap,
            }
            checks.append(pair_check)
            if overlap > 0:
                violations.append(
                    {
                        "type": "description_speech_overlap",
                        **pair_check,
                        "conflict_objects": [speech_item.item_id],
                        "minimum_duration_samples": desc.minimum_duration,
                        "actual_duration_samples": desc.duration,
                        "description_timecode": [
                            samples_to_timecode(desc.start, bundle.sample_rate),
                            samples_to_timecode(desc.end, bundle.sample_rate),
                        ],
                        "speech_timecode": [
                            samples_to_timecode(speech_item.start, bundle.sample_rate),
                            samples_to_timecode(speech_item.end, bundle.sample_rate),
                        ],
                    }
                )
            elif not passed:
                violations.append(
                    {
                        "type": "description_minimum_gap",
                        **pair_check,
                        "conflict_objects": [speech_item.item_id],
                        "minimum_duration_samples": desc.minimum_duration,
                        "actual_duration_samples": desc.duration,
                    }
                )

        descriptions.append(
            {
                "description_id": desc.item_id,
                "text": desc.text,
                "speaker": desc.speaker,
                "status": desc.status,
                "readable": desc.readable,
                "decision": desc.decision,
                "interval": [desc.start, desc.end],
                "timecode": [
                    samples_to_timecode(desc.start, bundle.sample_rate),
                    samples_to_timecode(desc.end, bundle.sample_rate),
                ],
                "minimum_duration_samples": desc.minimum_duration,
                "actual_duration_samples": desc.duration,
                "text_length": source.text_length,
                "max_reading_rate_cps": str(source.max_cps),
                "min_gap_samples": source.min_gap_samples,
                "conflict_objects": sorted(
                    set(desc.depends_on)
                    | {
                        conflict.get("other_id", "")
                        for conflict in desc.conflicts
                        if conflict.get("other_id")
                    }
                ),
                "checks": checks,
            }
        )

    report = ComplianceReport(
        material_id=bundle.material_id,
        version=result.version,
        sample_rate=bundle.sample_rate,
        result_hash=result.result_hash,
        rules=rules,
        descriptions=descriptions,
        violations=violations,
        compliant=not violations,
    )
    report.finalize_hash()
    return report


def build_tracks(
    bundle: SourceBundle,
    result: SolveResult,
) -> tuple[SpeechTrack, DescriptionTrack]:
    speech_track = SpeechTrack(
        material_id=bundle.material_id,
        version=result.version,
        sample_rate=bundle.sample_rate,
        result_hash=result.result_hash,
        items=items_to_track(
            result.items,
            bundle.sample_rate,
            bundle.material_id,
            result.version,
            result.result_hash,
            "speech",
        ),
    )
    description_track = DescriptionTrack(
        material_id=bundle.material_id,
        version=result.version,
        sample_rate=bundle.sample_rate,
        result_hash=result.result_hash,
        items=items_to_track(
            result.items,
            bundle.sample_rate,
            bundle.material_id,
            result.version,
            result.result_hash,
            "description",
        ),
    )
    return speech_track, description_track


def speaker_markers(result: SolveResult) -> list[dict[str, Any]]:
    markers: list[dict[str, Any]] = []
    for item in sorted(result.items, key=lambda value: (value.start, value.item_id)):
        if item.status == "rejected":
            continue
        markers.append(
            {
                "sentence_id": item.item_id,
                "speaker": item.speaker,
                "kind": item.kind,
                "start_sample": item.start,
                "end_sample": item.end,
            }
        )
    return markers


def item_digest(item: ItemResult) -> str:
    return content_hash(item.canonical(include_hash=False))
