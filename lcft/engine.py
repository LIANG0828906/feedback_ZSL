"""Deterministic speech/description layout and incremental re-layout."""

from __future__ import annotations

from copy import deepcopy
from typing import Any, Iterable

from .canonical import sha256_value, without_keys
from .errors import LCFTError
from .estimator import (
    estimate_param_hash,
    estimate_sentence,
    estimates_equal_for_reuse,
    input_hash,
)
from .hashchain import build_chain, seal_item
from .params import ESTIMATOR_VERSION, LayoutParams
from .timebase import NormalizedSubtitle, Timebase, TimebaseResult

IE = "E_TIMELINE_CONFLICT"


def _unique(values: Iterable[str]) -> list[str]:
    result: list[str] = []
    seen: set[str] = set()
    for value in values:
        if value not in seen:
            seen.add(value)
            result.append(value)
    return result


def _reason(rule: str, **values: Any) -> dict[str, Any]:
    return {"rule": rule, **values}


def _frame_fields(row: dict[str, Any], timebase: Timebase) -> None:
    start = row.get("start_ticks")
    end = row.get("end_ticks")
    if start is None or end is None:
        row["frame_start"] = None
        row["frame_end"] = None
        row["frame_interval"] = None
        row["start_timecode"] = None
        row["end_timecode"] = None
        return
    frame_start, frame_end = timebase.frames.frame_interval(start, end)
    row["frame_start"] = frame_start
    row["frame_end"] = frame_end
    row["frame_interval"] = [frame_start, frame_end]
    row["start_timecode"] = timebase.timebase.to_timecode(start)
    row["end_timecode"] = timebase.timebase.to_timecode(end)


def _speech_estimate_view(estimate: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "id",
        "order",
        "kind",
        "text_hash",
        "input_hash",
        "param_estimate_hash",
        "readable_units",
        "max_read_speed",
        "ideal_ticks",
        "min_ticks",
        "target_ticks",
        "original_start_ticks",
        "original_end_ticks",
        "speaker",
        "priority",
        "original_frame_start",
        "original_frame_end",
        "estimate_hash",
    )
    return {key: estimate[key] for key in keys if key in estimate}


def _place_speech(
    estimate: dict[str, Any],
    *,
    previous_end: int | None,
    gap_ticks: int,
    total_samples: int,
) -> dict[str, Any]:
    original_start = estimate["original_start_ticks"]
    original_end = estimate["original_end_ticks"]
    original_duration = original_end - original_start
    required = estimate["min_ticks"]
    target = estimate["target_ticks"]
    start = original_start
    if previous_end is not None:
        start = max(original_start, previous_end + gap_ticks)

    effects: list[str] = []
    reasons: list[dict[str, Any]] = []
    rules: list[str] = []

    if start > original_start:
        effects.append("deferred")
        rules.append("R3_MIN_GAP_SHIFT")
        reasons.append(
            _reason(
                "R3_MIN_GAP_SHIFT",
                original_start=original_start,
                start=start,
                shift_ticks=start - original_start,
                min_gap_ticks=gap_ticks,
            )
        )
    if start >= total_samples:
        raise LCFTError(
            IE,
            "speech sentence starts beyond the media duration",
            field=f"sentences.{estimate['id']}.start",
            details={
                "sentence_id": estimate["id"],
                "start": start,
                "total_samples": total_samples,
            },
        )

    duration = target
    if start + duration > total_samples:
        available = total_samples - start
        if available < required:
            raise LCFTError(
                IE,
                "speech cannot fit before the media end without violating readability",
                field=f"sentences.{estimate['id']}.end",
                details={
                    "sentence_id": estimate["id"],
                    "start": start,
                    "available_ticks": available,
                    "required_ticks": required,
                },
            )
        duration = available
        effects.append("compressed")
        rules.append("R4_MEDIA_BOUND")
        reasons.append(
            _reason(
                "R4_MEDIA_BOUND",
                requested_ticks=target,
                awarded_ticks=duration,
                trimmed_ticks=target - duration,
                total_samples=total_samples,
            )
        )
    if duration > original_duration:
        effects.append("stretched")
        if estimate["ideal_ticks"] > original_duration:
            rules.append("R1_READ_SPEED")
            reasons.append(
                _reason(
                    "R1_READ_SPEED",
                    original_ticks=original_duration,
                    awarded_ticks=duration,
                    ideal_ticks=estimate["ideal_ticks"],
                    readable_units=estimate["readable_units"],
                    max_read_speed=estimate["max_read_speed"],
                )
            )
        if estimate["min_ticks"] > original_duration:
            rules.append("R2_MIN_DISPLAY")
            reasons.append(
                _reason(
                    "R2_MIN_DISPLAY",
                    original_ticks=original_duration,
                    awarded_ticks=duration,
                    min_ticks=estimate["min_ticks"],
                )
            )
    elif duration < original_duration:
        effects.append("compressed")

    effects = _unique(effects)
    rules = _unique(rules)
    end = start + duration
    if "compressed" in effects:
        verdict = "compressed"
    elif "stretched" in effects:
        verdict = "stretched"
    elif "deferred" in effects:
        verdict = "deferred"
    else:
        verdict = "unchanged"
    row: dict[str, Any] = {
        "id": estimate["id"],
        "order": estimate["order"],
        "kind": "speech",
        "text_hash": estimate["text_hash"],
        "input_hash": estimate["input_hash"],
        "param_estimate_hash": estimate["param_estimate_hash"],
        "estimate_hash": estimate["estimate_hash"],
        "readable_units": estimate["readable_units"],
        "max_read_speed": estimate["max_read_speed"],
        "ideal_ticks": estimate["ideal_ticks"],
        "min_ticks": estimate["min_ticks"],
        "target_ticks": estimate["target_ticks"],
        "original_start_ticks": original_start,
        "original_end_ticks": original_end,
        "original_duration_ticks": original_duration,
        "original_frame_start": estimate["original_frame_start"],
        "original_frame_end": estimate["original_frame_end"],
        "start_ticks": start,
        "end_ticks": end,
        "duration_ticks": duration,
        "speaker": estimate["speaker"],
        "priority": estimate["priority"],
        "verdict": verdict,
        "effects": effects,
        "rules": rules,
        "reasons": reasons,
    }
    return row


def _free_intervals(
    occupied: list[dict[str, Any]],
    total_samples: int,
    gap_ticks: int,
) -> list[tuple[int, int]]:
    result: list[tuple[int, int]] = []
    cursor = 0
    active = [row for row in occupied if row.get("start_ticks") is not None]
    for row in sorted(active, key=lambda item: (item["start_ticks"], item["id"])):
        start = row["start_ticks"]
        end = row["end_ticks"]
        if start < 0 or end < start:
            continue
        free_end = start - gap_ticks
        if free_end > cursor:
            result.append((cursor, free_end))
        cursor = max(cursor, end + gap_ticks)
    if total_samples > cursor:
        result.append((cursor, total_samples))
    return result


def _closest_start(
    interval: tuple[int, int],
    required: int,
    original_start: int,
) -> int:
    low, high = interval
    latest = high - required
    return min(max(original_start, low), latest)


def _description_estimate_view(estimate: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "id",
        "order",
        "kind",
        "text_hash",
        "input_hash",
        "param_estimate_hash",
        "readable_units",
        "max_read_speed",
        "ideal_ticks",
        "min_ticks",
        "target_ticks",
        "original_start_ticks",
        "original_end_ticks",
        "speaker",
        "priority",
        "original_frame_start",
        "original_frame_end",
        "estimate_hash",
    )
    return {key: estimate[key] for key in keys if key in estimate}


def _description_row(
    estimate: dict[str, Any],
    *,
    start: int | None,
    duration: int,
    verdict: str,
    rules: list[str],
    reasons: list[dict[str, Any]],
    placement_order: int,
) -> dict[str, Any]:
    end = None if start is None else start + duration
    return {
        "id": estimate["id"],
        "order": estimate["order"],
        "placement_order": placement_order,
        "kind": "description",
        "text_hash": estimate["text_hash"],
        "input_hash": estimate["input_hash"],
        "param_estimate_hash": estimate["param_estimate_hash"],
        "estimate_hash": estimate["estimate_hash"],
        "readable_units": estimate["readable_units"],
        "max_read_speed": estimate["max_read_speed"],
        "ideal_ticks": estimate["ideal_ticks"],
        "min_ticks": estimate["min_ticks"],
        "target_ticks": estimate["target_ticks"],
        "original_start_ticks": estimate["original_start_ticks"],
        "original_end_ticks": estimate["original_end_ticks"],
        "original_duration_ticks": estimate["original_end_ticks"]
        - estimate["original_start_ticks"],
        "original_frame_start": estimate["original_frame_start"],
        "original_frame_end": estimate["original_frame_end"],
        "start_ticks": start,
        "end_ticks": end,
        "duration_ticks": 0 if start is None else duration,
        "speaker": estimate["speaker"],
        "priority": estimate["priority"],
        "verdict": verdict,
        "effects": [verdict] if verdict != "unchanged" else [],
        "rules": _unique(rules),
        "reasons": reasons,
    }


def _conflict(
    description_id: str,
    participants: list[str],
    requested: int,
    available: int,
    reason: str,
) -> LCFTError:
    return LCFTError(
        "E_DESC_CONFLICT",
        "description placement cannot be adjudicated by priority",
        field=f"descriptions.{description_id}",
        details={
            "description_id": description_id,
            "participants": sorted(set(participants)),
            "requested_ticks": requested,
            "available_ticks": available,
            "reason": reason,
        },
    )


def _apply_description_arbitration(
    speech_rows: list[dict[str, Any]],
    description_estimates: list[dict[str, Any]],
    *,
    gap_ticks: int,
    total_samples: int,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[dict[str, Any]], list[dict[str, Any]]]:
    placed_rows: list[dict[str, Any]] = []
    trims: list[dict[str, Any]] = []
    edges: list[dict[str, Any]] = []

    priority_order = sorted(
        description_estimates,
        key=lambda estimate: (
            -int(estimate["priority"] if estimate["priority"] is not None else 2),
            estimate["original_start_ticks"],
            estimate["order"],
            estimate["id"],
        ),
    )
    placement_order = 0
    for estimate in priority_order:
        required = max(1, int(estimate["min_ticks"]))
        original_start = int(estimate["original_start_ticks"])
        occupied = list(speech_rows) + placed_rows
        intervals = _free_intervals(occupied, total_samples, gap_ticks)
        overlapping = [
            row
            for row in speech_rows
            if row["start_ticks"] < estimate["original_end_ticks"]
            and row["end_ticks"] > estimate["original_start_ticks"]
        ]

        natural: list[tuple[int, tuple[int, int]]] = []
        for interval in intervals:
            if interval[1] - interval[0] >= required:
                start = _closest_start(interval, required, original_start)
                score = (abs(start - original_start), interval[0], interval[1])
                natural.append((score, interval))
        natural.sort(key=lambda pair: pair[0])

        if natural:
            start = _closest_start(
                natural[0][1], required, original_start
            )
            rules = ["D3_NATURAL_GAP"]
            reasons = [
                _reason(
                    "D3_NATURAL_GAP",
                    interval_start=natural[0][1][0],
                    interval_end=natural[0][1][1],
                    requested_ticks=required,
                    start=start,
                )
            ]
            placed_rows.append(
                _description_row(
                    estimate,
                    start=start,
                    duration=required,
                    verdict="placed",
                    rules=rules,
                    reasons=reasons,
                    placement_order=placement_order,
                )
            )
            placement_order += 1
            continue

        candidates: list[dict[str, Any]] = []
        predecessor_by_low = {
            row["end_ticks"] + gap_ticks: row for row in speech_rows
        }
        for interval in intervals:
            low, high = interval
            if (
                high < estimate["original_start_ticks"] - required
                or low > estimate["original_end_ticks"] + required
            ):
                continue
            predecessor = predecessor_by_low.get(low)
            if predecessor is None:
                continue
            slack = max(0, predecessor["duration_ticks"] - predecessor["min_ticks"])
            needed = max(0, required - (high - low))
            distance = abs(low - original_start)
            candidates.append(
                {
                    "interval": interval,
                    "predecessor": predecessor,
                    "slack": slack,
                    "needed": needed,
                    "distance": distance,
                }
            )
        candidates.sort(
            key=lambda item: (
                item["distance"],
                item["interval"][0],
                item["predecessor"]["order"],
            )
        )
        candidate_ids = {item["predecessor"]["id"] for item in candidates}
        for row in overlapping:
            if row["id"] in candidate_ids:
                continue
            slack = max(0, row["duration_ticks"] - row["min_ticks"])
            candidates.append(
                {
                    "interval": (
                        row["end_ticks"] + gap_ticks,
                        row["end_ticks"] + gap_ticks + required,
                    ),
                    "predecessor": row,
                    "slack": slack,
                    "needed": required,
                    "distance": abs(
                        row["start_ticks"] - estimate["original_start_ticks"]
                    ),
                }
            )
        candidates.sort(
            key=lambda item: (
                item["distance"],
                item["interval"][0],
                item["predecessor"]["order"],
            )
        )

        chosen: dict[str, Any] | None = None
        for candidate in candidates:
            predecessor = candidate["predecessor"]
            predecessor_priority = int(
                predecessor["priority"] if predecessor["priority"] is not None else 3
            )
            if int(estimate["priority"]) > predecessor_priority and candidate[
                "slack"
            ] >= candidate["needed"]:
                chosen = candidate
                break

        if chosen is not None:
            predecessor = chosen["predecessor"]
            needed = chosen["needed"]
            predecessor["end_ticks"] -= needed
            predecessor["duration_ticks"] -= needed
            predecessor["effects"] = _unique(
                list(predecessor.get("effects", [])) + ["compressed"]
            )
            predecessor["rules"] = _unique(
                list(predecessor.get("rules", [])) + ["D1_DESCRIPTION_FIT"]
            )
            predecessor["reasons"] = list(predecessor.get("reasons", [])) + [
                _reason(
                    "D1_DESCRIPTION_FIT",
                    description_id=estimate["id"],
                    trimmed_ticks=needed,
                    awarded_ticks=predecessor["duration_ticks"],
                    min_ticks=predecessor["min_ticks"],
                )
            ]
            if predecessor["duration_ticks"] > predecessor["original_duration_ticks"]:
                predecessor["verdict"] = "stretched"
            else:
                predecessor["verdict"] = "compressed"
            trims.append(
                {
                    "description_id": estimate["id"],
                    "target_id": predecessor["id"],
                    "kind": "speech",
                    "rule": "D1_DESCRIPTION_FIT",
                    "participants": [estimate["id"], predecessor["id"]],
                    "requested_ticks": needed,
                    "awarded_ticks": needed,
                    "trimmed_ticks": needed,
                }
            )
            edges.append(
                {
                    "from": predecessor["id"],
                    "to": estimate["id"],
                    "kind": "description_trim",
                    "contention_ticks": needed,
                }
            )
            occupied = list(speech_rows) + placed_rows
            intervals = _free_intervals(occupied, total_samples, gap_ticks)
            fitting = [
                (abs(_closest_start(interval, required, original_start) - original_start),
                 interval)
                for interval in intervals
                if interval[1] - interval[0] >= required
            ]
            if not fitting:
                raise _conflict(
                    estimate["id"],
                    [estimate["id"], predecessor["id"]],
                    required,
                    max((high - low for low, high in intervals), default=0),
                    "trimmed_speech_but_no_contiguous_gap",
                )
            fitting.sort(key=lambda pair: pair[0])
            start = _closest_start(fitting[0][1], required, original_start)
            rules = ["D1_DESCRIPTION_FIT", "D4_PRIORITY_PLACEMENT"]
            reasons = [
                _reason(
                    "D1_DESCRIPTION_FIT",
                    target_id=predecessor["id"],
                    trimmed_ticks=needed,
                    requested_ticks=required,
                ),
                _reason(
                    "D4_PRIORITY_PLACEMENT",
                    description_priority=estimate["priority"],
                    speech_priority=predecessor["priority"],
                    start=start,
                ),
            ]
            placed_rows.append(
                _description_row(
                    estimate,
                    start=start,
                    duration=required,
                    verdict="placed",
                    rules=rules,
                    reasons=reasons,
                    placement_order=placement_order,
                )
            )
            placement_order += 1
            continue

        overlapping_descriptions = [
            row
            for row in placed_rows
            if row.get("start_ticks") is not None
            and row["start_ticks"] < estimate["original_end_ticks"]
            and row["end_ticks"] > estimate["original_start_ticks"]
        ]
        relevant = candidates if candidates else []
        contenders: dict[str, dict[str, Any]] = {}
        for item in relevant:
            predecessor = item["predecessor"]
            contenders[predecessor["id"]] = predecessor
        for row in overlapping:
            contenders.setdefault(row["id"], row)
        for row in overlapping_descriptions:
            contenders.setdefault(row["id"], row)
        ordered_contenders = sorted(
            contenders.values(),
            key=lambda row: (
                abs(row["start_ticks"] - estimate["original_start_ticks"]),
                row["order"],
                row["id"],
            ),
        )
        priorities = [
            int(row["priority"] if row["priority"] is not None else 3)
            for row in ordered_contenders
        ]
        description_priority = int(estimate["priority"])
        if priorities and all(description_priority < value for value in priorities):
            contender = ordered_contenders[0]
            speech_id = contender["id"]
            candidate_match = next(
                (
                    item
                    for item in relevant
                    if item["predecessor"]["id"] == speech_id
                ),
                None,
            )
            contention_ticks = (
                candidate_match["needed"]
                if candidate_match is not None
                else required
            )
            trims.append(
                {
                    "description_id": estimate["id"],
                    "target_id": estimate["id"],
                    "kind": "description",
                    "rule": "D2_PRIORITY_LOSER",
                    "participants": [estimate["id"]]
                    + sorted(contenders),
                    "requested_ticks": required,
                    "awarded_ticks": 0,
                    "trimmed_ticks": 0,
                    "contention_ticks": max(0, contention_ticks),
                }
            )
            edges.append(
                {
                    "from": speech_id,
                    "to": estimate["id"],
                    "kind": "description_priority_rejected",
                    "contention_ticks": max(0, contention_ticks),
                }
            )
            placed_rows.append(
                _description_row(
                    estimate,
                    start=None,
                    duration=0,
                    verdict="rejected",
                    rules=["D2_PRIORITY_LOSER"],
                    reasons=[
                        _reason(
                            "D2_PRIORITY_LOSER",
                            description_priority=description_priority,
                            speech_priority=contender["priority"],
                            requested_ticks=required,
                        )
                    ],
                    placement_order=placement_order,
                )
            )
            placement_order += 1
            continue

        participants = [estimate["id"]]
        participants.extend(row["id"] for row in overlapping)
        participants.extend(row["id"] for row in overlapping_descriptions)
        participants.extend(contenders)
        raise _conflict(
            estimate["id"],
            participants,
            required,
            max((high - low for low, high in intervals), default=0),
            "equal_or_indeterminate_priority",
        )

    by_input_order = sorted(placed_rows, key=lambda row: row["order"])
    result_trims = sorted(
        trims,
        key=lambda item: (
            item["kind"],
            item["description_id"],
            item["target_id"],
        ),
    )
    result_edges = sorted(
        edges,
        key=lambda item: (item["from"], item["to"], item["kind"]),
    )
    return by_input_order, result_trims, result_edges, speech_rows


def _speaker_marks(
    speech_rows: list[dict[str, Any]],
    timebase: Timebase,
) -> list[dict[str, Any]]:
    marks: list[dict[str, Any]] = []
    for index in range(1, len(speech_rows)):
        previous = speech_rows[index - 1]
        current = speech_rows[index]
        if previous["speaker"] == current["speaker"]:
            continue
        mark = {
            "id": f"mark-{previous['id']}-{current['id']}",
            "after_sentence_id": previous["id"],
            "before_sentence_id": current["id"],
            "from_speaker": previous["speaker"],
            "to_speaker": current["speaker"],
            "at_ticks": current["start_ticks"],
            "at_timecode": timebase.timebase.to_timecode(current["start_ticks"]),
        }
        marks.append(seal_item(mark))
    return marks


def _build_report(
    *,
    track_id: str,
    language: str,
    sentences: list[dict[str, Any]],
    descriptions: list[dict[str, Any]],
    trims: list[dict[str, Any]],
    contention_edges: list[dict[str, Any]],
    timebase: TimebaseResult,
    params: LayoutParams,
    published_version: int,
    param_version: int,
) -> dict[str, Any]:
    seen_ids: set[str] = set()
    for row in sentences + descriptions:
        if row["id"] in seen_ids:
            raise LCFTError(
                "E_INPUT_INVALID",
                "sentence and description ids must be unique within a track",
                field="sentences.id",
                details={"id": row["id"]},
            )
        seen_ids.add(row["id"])
    for row in sentences + descriptions:
        _frame_fields(row, timebase)
    sealed_sentences = [seal_item(row) for row in sentences]
    sealed_descriptions = [seal_item(row) for row in descriptions]
    marks = _speaker_marks(sealed_sentences, timebase)

    chain = build_chain(
        [("sentence", row) for row in sealed_sentences]
        + [("description", row) for row in sealed_descriptions]
        + [("mark", row) for row in marks]
    )
    report: dict[str, Any] = {
        "schema_version": 1,
        "kind": "track-layout",
        "track_id": track_id,
        "language": language,
        "published_version": published_version,
        "param_version": param_version,
        "params": params.to_dict(),
        "params_hash": params.hash,
        "estimator_version": ESTIMATOR_VERSION,
        "timebase": {
            "unit": "sample",
            "sample_rate": timebase.audio.sample_rate,
            "total_samples": timebase.audio.total_samples,
            "frame_rate": timebase.frames.to_dict()["frame_rate"],
            "frame_table_sha256": timebase.frames.sha256,
        },
        "sentences": sealed_sentences,
        "descriptions": sealed_descriptions,
        "speaker_marks": marks,
        "trims": trims,
        "contention_edges": contention_edges,
        "pending_sentence_ids": [],
        "chain": chain,
    }
    report["track_hash"] = sha256_value(
        {key: value for key, value in report.items() if key != "track_hash"}
    )
    return report


def _sorted_estimates(
    estimates: list[dict[str, Any]] | None,
    items: tuple[NormalizedSubtitle, ...],
    params: LayoutParams,
    timebase_result: TimebaseResult,
    *,
    description: bool,
) -> list[dict[str, Any]]:
    if estimates is None:
        rows = [
            estimate_sentence(
                item,
                params,
                timebase_result.audio.sample_rate,
                description=description,
                order=index,
            )
            for index, item in enumerate(items)
        ]
    else:
        expected_ids = [item.id for item in items]
        if [row["id"] for row in estimates] != expected_ids:
            raise LCFTError(
                "E_INPUT_INVALID",
                "precomputed row order does not match sentence order",
                field=("descriptions" if description else "sentences"),
            )
        rows = [deepcopy(row) for row in estimates]
    for row in rows:
        if row.get("priority") is None:
            row["priority"] = (
                params.description_priority if description else params.speech_priority
            )
    return rows


def layout_track(
    track_id: str,
    language: str,
    sentences: tuple[NormalizedSubtitle, ...] | list[NormalizedSubtitle],
    descriptions: tuple[NormalizedSubtitle, ...] | list[NormalizedSubtitle],
    timebase: TimebaseResult,
    params: LayoutParams | dict[str, Any] | None = None,
    *,
    published_version: int = 1,
    param_version: int = 1,
    precomputed_sentences: list[dict[str, Any]] | None = None,
    precomputed_descriptions: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Lay out one track.  Descriptions use the same integer rules as speech."""

    layout_params = params if isinstance(params, LayoutParams) else LayoutParams.from_dict(params)
    layout_params.validate()
    speech_items = tuple(sorted(sentences, key=lambda item: (item.start_sample, item.id)))
    description_items = tuple(
        sorted(descriptions, key=lambda item: (item.start_sample, item.id))
    )
    speech_estimates = _sorted_estimates(
        precomputed_sentences,
        speech_items,
        layout_params,
        timebase,
        description=False,
    )
    description_estimates = _sorted_estimates(
        precomputed_descriptions,
        description_items,
        layout_params,
        timebase,
        description=True,
    )
    gaps = layout_params.seconds_to_ticks(
        layout_params.min_gap_seconds, timebase.audio.sample_rate, mode="round"
    )
    speech_rows: list[dict[str, Any]] = []
    previous_end: int | None = None
    for estimate in speech_estimates:
        row = _place_speech(
            estimate,
            previous_end=previous_end,
            gap_ticks=gaps,
            total_samples=timebase.audio.total_samples,
        )
        speech_rows.append(row)
        previous_end = row["end_ticks"]
    descriptions_out, trims, edges, speech_rows = _apply_description_arbitration(
        speech_rows,
        description_estimates,
        gap_ticks=gaps,
        total_samples=timebase.audio.total_samples,
    )
    return _build_report(
        track_id=track_id,
        language=language,
        sentences=speech_rows,
        descriptions=descriptions_out,
        trims=trims,
        contention_edges=edges,
        timebase=timebase,
        params=layout_params,
        published_version=published_version,
        param_version=param_version,
    )


def _estimate_from_row(row: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "id",
        "order",
        "kind",
        "text_hash",
        "input_hash",
        "param_estimate_hash",
        "readable_units",
        "max_read_speed",
        "ideal_ticks",
        "min_ticks",
        "target_ticks",
        "original_start_ticks",
        "original_end_ticks",
        "speaker",
        "priority",
        "original_frame_start",
        "original_frame_end",
        "estimate_hash",
    )
    return {key: row[key] for key in keys if key in row}


def incremental_layout(
    previous_report: dict[str, Any],
    sentences: tuple[NormalizedSubtitle, ...] | list[NormalizedSubtitle],
    descriptions: tuple[NormalizedSubtitle, ...] | list[NormalizedSubtitle],
    timebase: TimebaseResult,
    params: LayoutParams | dict[str, Any] | None = None,
    *,
    param_version: int | None = None,
    published_version: int | None = None,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Re-solve the changed closure and reuse all provably unchanged rows."""

    layout_params = params if isinstance(params, LayoutParams) else LayoutParams.from_dict(params)
    layout_params.validate()
    speech_items = tuple(sorted(sentences, key=lambda item: (item.start_sample, item.id)))
    description_items = tuple(
        sorted(descriptions, key=lambda item: (item.start_sample, item.id))
    )
    old_sentences = previous_report.get("sentences")
    old_descriptions = previous_report.get("descriptions")
    old_params_raw = previous_report.get("params")
    if not isinstance(old_sentences, list) or not isinstance(old_descriptions, list):
        full = layout_track(
            previous_report.get("track_id", "speech"),
            previous_report.get("language", ""),
            speech_items,
            description_items,
            timebase,
            layout_params,
            published_version=published_version
            or int(previous_report.get("published_version", 0)) + 1,
            param_version=param_version
            or int(previous_report.get("param_version", 0)) + 1,
        )
        stats = {
            "affected_count": len(speech_items) + len(description_items),
            "reused_count": 0,
            "conservative": True,
            "reasons": ["previous report lacks reusable sentence/description rows"],
            "affected_ids": [item.id for item in speech_items]
            + [item.id for item in description_items],
            "track_hash": full["track_hash"],
        }
        return full, stats
    old_params = LayoutParams.from_dict(old_params_raw if isinstance(old_params_raw, dict) else {})
    old_by_id = {row["id"]: row for row in old_sentences}
    old_desc_by_id = {row["id"]: row for row in old_descriptions}

    reasons: list[str] = []
    conservative = False
    old_timebase = previous_report.get("timebase", {})
    if (
        not isinstance(old_timebase, dict)
        or old_timebase.get("sample_rate") != timebase.audio.sample_rate
        or old_timebase.get("total_samples") != timebase.audio.total_samples
        or old_timebase.get("frame_rate") != timebase.frames.to_dict()["frame_rate"]
        or old_timebase.get("frame_table_sha256") != timebase.frames.sha256
    ):
        conservative = True
        reasons.append("media duration or frame timetable changed")
    current_speech_order = [item.id for item in speech_items]
    old_speech_order = [row["id"] for row in old_sentences]
    removed_speech = sorted(set(old_speech_order) - set(current_speech_order))
    removed_desc = sorted(
        set(row["id"] for row in old_descriptions)
        - set(item.id for item in description_items)
    )
    if removed_speech or removed_desc:
        conservative = True
        reasons.append("sentence or description removal requires a conservative full layout")
    if current_speech_order != old_speech_order:
        conservative = True
        reasons.append("sentence order changed; suffix reuse cannot be proven")
    if [row["id"] for row in old_descriptions] != [
        item.id for item in description_items
    ]:
        conservative = True
        reasons.append("description order changed; placement reuse cannot be proven")
    if conservative:
        full = layout_track(
            previous_report.get("track_id", "speech"),
            previous_report.get("language", ""),
            speech_items,
            description_items,
            timebase,
            layout_params,
            published_version=published_version
            or int(previous_report.get("published_version", 0)) + 1,
            param_version=param_version
            or int(previous_report.get("param_version", 0)) + 1,
        )
        stats = {
            "affected_count": len(speech_items) + len(description_items),
            "reused_count": 0,
            "conservative": True,
            "reasons": reasons,
            "affected_ids": current_speech_order
            + [item.id for item in description_items],
            "track_hash": full["track_hash"],
        }
        return full, stats

    current_estimates: list[dict[str, Any]] = []
    speech_seeds: set[str] = set()
    for order, item in enumerate(speech_items):
        old_row = old_by_id.get(item.id)
        if old_row is None:
            estimate = estimate_sentence(
                item,
                layout_params,
                timebase.audio.sample_rate,
                description=False,
                order=order,
            )
            speech_seeds.add(item.id)
        else:
            current_input_hash = input_hash(item, description=False)
            current_param_hash = estimate_param_hash(
                item, layout_params, timebase.audio.sample_rate, description=False
            )
            if (
                old_row.get("input_hash") == current_input_hash
                and old_row.get("param_estimate_hash") == current_param_hash
            ):
                estimate = _estimate_from_row(old_row)
                estimate["order"] = order
            else:
                estimate = estimate_sentence(
                    item,
                    layout_params,
                    timebase.audio.sample_rate,
                    description=False,
                    order=order,
                )
                old_estimate = _estimate_from_row(old_row)
                if (
                    current_input_hash != old_row.get("input_hash")
                    or not estimates_equal_for_reuse(old_estimate, estimate)
                ):
                    speech_seeds.add(item.id)
        current_estimates.append(estimate)

    old_gap = old_params.seconds_to_ticks(
        old_params.min_gap_seconds, timebase.audio.sample_rate, mode="round"
    )
    new_gap = layout_params.seconds_to_ticks(
        layout_params.min_gap_seconds, timebase.audio.sample_rate, mode="round"
    )
    if new_gap != old_gap:
        for index in range(1, len(old_sentences)):
            previous = old_sentences[index - 1]
            current = old_sentences[index]
            if current["start_ticks"] - previous["end_ticks"] <= max(old_gap, new_gap):
                speech_seeds.add(current["id"])

    description_estimates: list[dict[str, Any]] = []
    description_seeds: set[str] = set()
    for order, item in enumerate(description_items):
        old_row = old_desc_by_id.get(item.id)
        if old_row is None:
            estimate = estimate_sentence(
                item,
                layout_params,
                timebase.audio.sample_rate,
                description=True,
                order=order,
            )
            description_seeds.add(item.id)
        else:
            current_input_hash = input_hash(item, description=True)
            current_param_hash = estimate_param_hash(
                item, layout_params, timebase.audio.sample_rate, description=True
            )
            if (
                old_row.get("input_hash") == current_input_hash
                and old_row.get("param_estimate_hash") == current_param_hash
            ):
                estimate = _estimate_from_row(old_row)
                estimate["order"] = order
            else:
                estimate = estimate_sentence(
                    item,
                    layout_params,
                    timebase.audio.sample_rate,
                    description=True,
                    order=order,
                )
                old_estimate = _estimate_from_row(old_row)
                if (
                    current_input_hash != old_row.get("input_hash")
                    or not estimates_equal_for_reuse(old_estimate, estimate)
                ):
                    description_seeds.add(item.id)
        description_estimates.append(estimate)

    affected: set[str] = set()
    solved_speech: list[dict[str, Any]] = []
    previous_end: int | None = None
    previous_changed = False
    for index, estimate in enumerate(current_estimates):
        old_row = old_by_id[estimate["id"]]
        seed = estimate["id"] in speech_seeds
        must_recompute = seed
        if not must_recompute and previous_changed:
            candidate_start = max(
                estimate["original_start_ticks"],
                (previous_end if previous_end is not None else 0) + new_gap,
            )
            if candidate_start != old_row["start_ticks"]:
                must_recompute = True
        if not must_recompute:
            row = deepcopy(old_row)
            row["order"] = index
            solved_speech.append(row)
            previous_end = row["end_ticks"]
            previous_changed = False
            continue
        row = _place_speech(
            estimate,
            previous_end=previous_end,
            gap_ticks=new_gap,
            total_samples=timebase.audio.total_samples,
        )
        affected.add(row["id"])
        previous_changed = (
            row["start_ticks"] != old_row["start_ticks"]
            or row["end_ticks"] != old_row["end_ticks"]
        )
        solved_speech.append(row)
        previous_end = row["end_ticks"]

    descriptions_out: list[dict[str, Any]]
    trims: list[dict[str, Any]]
    edges: list[dict[str, Any]]
    rerun_descriptions = bool(
        affected or description_seeds or new_gap != old_gap or not old_desc_by_id
    )
    if rerun_descriptions:
        old_trim_targets = {
            item.get("target_id")
            for item in previous_report.get("trims", [])
            if item.get("kind") == "speech"
        }
        if old_trim_targets:
            previous_end = None
            for index, row in enumerate(solved_speech):
                if row["id"] in old_trim_targets:
                    restored = _place_speech(
                        current_estimates[index],
                        previous_end=previous_end,
                        gap_ticks=new_gap,
                        total_samples=timebase.audio.total_samples,
                    )
                    if without_keys(restored, ("content_hash",)) != without_keys(
                        row, ("content_hash",)
                    ):
                        affected.add(row["id"])
                    solved_speech[index] = restored
                    row = restored
                previous_end = row["end_ticks"]

    if not rerun_descriptions:
        descriptions_out = [deepcopy(row) for row in old_descriptions]
        trims = deepcopy(previous_report.get("trims", []))
        edges = deepcopy(previous_report.get("contention_edges", []))
    else:
        descriptions_out, trims, edges, solved_speech = _apply_description_arbitration(
            solved_speech,
            description_estimates,
            gap_ticks=new_gap,
            total_samples=timebase.audio.total_samples,
        )
        for older, newer in zip(old_sentences, solved_speech):
            if older["id"] != newer["id"]:
                conservative = True
                reasons.append("sentence order changed during incremental layout")
                break
            old_core = without_keys(older, ("content_hash",))
            new_core = without_keys(newer, ("content_hash",))
            if old_core != new_core:
                affected.add(newer["id"])
        affected.update(description_seeds)
        if description_seeds:
            affected.update(row["id"] for row in descriptions_out)
        else:
            affected.update(row["id"] for row in descriptions_out)

    if conservative:
        full = layout_track(
            previous_report.get("track_id", "speech"),
            previous_report.get("language", ""),
            speech_items,
            description_items,
            timebase,
            layout_params,
            published_version=published_version
            or int(previous_report.get("published_version", 0)) + 1,
            param_version=param_version
            or int(previous_report.get("param_version", 0)) + 1,
        )
        stats = {
            "affected_count": len(speech_items) + len(description_items),
            "reused_count": 0,
            "conservative": True,
            "reasons": _unique(reasons or ["dependency proof failed"]),
            "affected_ids": current_speech_order
            + [item.id for item in description_items],
            "track_hash": full["track_hash"],
        }
        return full, stats

    next_version = (
        published_version
        if published_version is not None
        else int(previous_report.get("published_version", 1)) + 1
    )
    next_param_version = (
        param_version
        if param_version is not None
        else int(previous_report.get("param_version", 1))
    )
    report = _build_report(
        track_id=previous_report.get("track_id", "speech"),
        language=previous_report.get("language", ""),
        sentences=solved_speech,
        descriptions=descriptions_out,
        trims=trims,
        contention_edges=edges,
        timebase=timebase,
        params=layout_params,
        published_version=next_version,
        param_version=next_param_version,
    )
    affected_ids = [
        row["id"] for row in report["sentences"] if row["id"] in affected
    ] + [row["id"] for row in report["descriptions"] if row["id"] in affected]
    total = len(report["sentences"]) + len(report["descriptions"])
    if not reasons:
        reasons = (
            ["propagated through integer gap/shift dependencies"]
            if affected_ids
            else ["no estimate or constraint changed; all rows reused"]
        )
    stats = {
        "affected_count": len(affected_ids),
        "reused_count": max(0, total - len(affected_ids)),
        "conservative": False,
        "reasons": reasons,
        "affected_ids": affected_ids,
        "track_hash": report["track_hash"],
    }
    return report, stats

