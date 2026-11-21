from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any, Callable, Iterable, Mapping, Sequence

from .errors import (
    BusinessError,
    CancelledError,
    E_DESC_CONFLICT,
    E_LAYOUT_MISMATCH,
    E_TIMEBASE_INVALID,
)
from .hashing import content_hash, hash_without, sorted_unique
from .models import (
    DescriptionInput,
    LayoutParams,
    SCHEMA_REPORT,
    SentenceInput,
    SubtitleAsset,
)
from .timebase import (
    FrameTable,
    SAMPLE_RATE,
    SentenceTiming,
    WavInfo,
    format_timecode,
    ms_to_samples,
    parse_timecode,
    samples_to_ticks,
    ticks_to_samples,
    validate_media_and_asset,
)


ZERO_HASH = "0" * 64


@dataclass(frozen=True)
class Checkpoint:
    sentence_id: str
    entry: Mapping[str, Any]
    affected_ids: tuple[str, ...]
    reused_ids: tuple[str, ...]


CheckpointCallback = Callable[[Checkpoint], None]


def effective_cps(sentence: SentenceInput, params: LayoutParams) -> str:
    return sentence.cps if sentence.cps is not None else params.default_cps


def duration_for_text(text: str, cps: str) -> int:
    speed = Fraction(cps)
    if speed <= 0:
        raise BusinessError(
            E_TIMEBASE_INVALID,
            "reading speed must be positive",
            {"field": "cps", "value": cps},
        )
    numerator = len(text) * SAMPLE_RATE * speed.denominator
    return (numerator + speed.numerator - 1) // speed.numerator


def _sentence_fingerprint(
    sentence: SentenceInput,
    params: LayoutParams,
    timing: SentenceTiming,
) -> str:
    channel = params.channel_map.get(sentence.speaker, params.default_channel)
    return content_hash(
        {
            "channel": channel,
            "cps": effective_cps(sentence, params),
            "orig_end_sample": timing.orig_end_sample,
            "orig_start_sample": timing.orig_start_sample,
            "sentence": sentence.to_dict(),
            "min_duration_ms": params.min_duration_ms,
        }
    )


def _verdict(
    *,
    orig_start: int,
    orig_end: int,
    start: int,
    end: int,
    desired: int,
    min_duration_samples: int,
    compressed_reasons: Iterable[str] = (),
    description_cut: bool = False,
) -> dict[str, Any]:
    duration = end - start
    reasons: list[str] = []
    compressed = False
    extended = False
    deferred = start > orig_start
    for reason in compressed_reasons:
        if reason not in reasons:
            reasons.append(reason)
            compressed = True
    if duration < desired and "media_end_clip" not in reasons and "description_duration" not in reasons:
        # A description can only shrink a track when the caller explicitly
        # accepts the compressed description.  Ordinary text is never silently
        # shortened below its reading-speed requirement.
        if "description_priority_cut" not in compressed_reasons:
            compressed = False
    if deferred:
        reasons.append("min_gap_defer")
    if description_cut:
        if "description_priority_cut" not in reasons:
            reasons.append("description_priority_cut")
        deferred = True
        compressed = True
    original_duration = orig_end - orig_start
    if duration > original_duration:
        extended = True
        if desired > original_duration and "reading_speed_limit" not in reasons:
            reasons.append("reading_speed_limit")
    if min_duration_samples > desired and duration >= min_duration_samples:
        extended = True
        if "minimum_duration" not in reasons:
            reasons.append("minimum_duration")
    if deferred:
        status = "deferred"
    elif compressed:
        status = "compressed"
    elif extended:
        status = "extended"
    else:
        status = "kept"
    return {
        "compressed": compressed,
        "deferred": deferred,
        "extended": extended,
        "reasons": reasons,
        "status": status,
    }


def _finalize_entry(entry: dict[str, Any]) -> dict[str, Any]:
    result = dict(entry)
    result["entry_hash"] = hash_without(result, "entry_hash")
    return result


def _finalize_description(description: dict[str, Any]) -> dict[str, Any]:
    result = dict(description)
    result["description_hash"] = hash_without(result, "description_hash")
    return result


def _finalize_marker(marker: dict[str, Any]) -> dict[str, Any]:
    result = dict(marker)
    result["marker_hash"] = hash_without(result, "marker_hash")
    return result


def _description_duration(description: DescriptionInput) -> tuple[int, int]:
    speed_duration = duration_for_text(description.text, description.max_cps)
    minimum = max(1, ms_to_samples(description.min_duration_ms))
    return max(speed_duration, minimum), minimum


def _make_sentence_entry(
    sentence: SentenceInput,
    timing: SentenceTiming,
    params: LayoutParams,
    *,
    start: int,
    duration: int,
    desired: int,
    min_duration_samples: int,
    clipped: bool,
    description_cut: bool,
) -> dict[str, Any]:
    end = start + duration
    compressed_reasons: list[str] = []
    if clipped:
        compressed_reasons.append("media_end_clip")
    verdict = _verdict(
        orig_start=timing.orig_start_sample,
        orig_end=timing.orig_end_sample,
        start=start,
        end=end,
        desired=desired,
        min_duration_samples=min_duration_samples,
        compressed_reasons=compressed_reasons,
        description_cut=description_cut,
    )
    entry = {
        "cps": effective_cps(sentence, params),
        "desired_duration_samples": desired,
        "end": format_timecode(samples_to_ticks(end)),
        "end_sample": end,
        "frame_end": 0,
        "frame_start": 0,
        "input_fingerprint": _sentence_fingerprint(sentence, params, timing),
        "orig_end_sample": timing.orig_end_sample,
        "orig_start_sample": timing.orig_start_sample,
        "sentence_id": sentence.id,
        "speaker": sentence.speaker,
        "start": format_timecode(samples_to_ticks(start)),
        "start_sample": start,
        "text": sentence.text,
        "verdict": verdict,
    }
    return _finalize_entry(entry)


def _entry_with_frames(entry: Mapping[str, Any], frames: FrameTable) -> dict[str, Any]:
    result = dict(entry)
    start_frame, end_frame = frames.frame_interval(
        int(result["start_sample"]), int(result["end_sample"])
    )
    result["frame_start"] = start_frame
    result["frame_end"] = end_frame
    return _finalize_entry(result)


def _entry_matches_sentence(
    entry: Mapping[str, Any],
    sentence: SentenceInput,
    params: LayoutParams,
    timing: SentenceTiming,
) -> bool:
    return (
        entry.get("sentence_id") == sentence.id
        and entry.get("input_fingerprint") == _sentence_fingerprint(sentence, params, timing)
        and entry.get("orig_start_sample") == timing.orig_start_sample
        and entry.get("orig_end_sample") == timing.orig_end_sample
    )


def _next_sentence_input(
    sentences: Sequence[SentenceInput],
    sentence_id: str,
) -> SentenceInput | None:
    for index, sentence in enumerate(sentences):
        if sentence.id == sentence_id:
            return sentences[index + 1] if index + 1 < len(sentences) else None
    return None


def _normalise_descriptions_by_anchor(
    descriptions: Sequence[DescriptionInput],
    sentence_order: Sequence[str],
) -> dict[str, list[DescriptionInput]]:
    order = {sentence_id: index for index, sentence_id in enumerate(sentence_order)}
    result: dict[str, list[DescriptionInput]] = {sentence_id: [] for sentence_id in sentence_order}
    for description in descriptions:
        result.setdefault(description.after_sentence_id, []).append(description)
    for values in result.values():
        values.sort(key=lambda item: (item.id,))
    # Anchors are validated elsewhere, but an unknown anchor must not be
    # silently dropped.
    unknown = sorted(set(result) - set(order))
    if unknown:
        raise BusinessError(
            "E_INVALID_INPUT",
            "description anchor sentence does not exist",
            {"after_sentence_id": unknown[0]},
        )
    return result


def _build_speaker_markers(entries: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    markers: list[dict[str, Any]] = []
    previous_speaker: str | None = None
    for entry in entries:
        speaker = str(entry["speaker"])
        if previous_speaker is None or speaker != previous_speaker:
            markers.append(
                _finalize_marker(
                    {
                        "at_sentence_id": entry["sentence_id"],
                        "sample": entry["start_sample"],
                        "speaker": speaker,
                        "timecode": entry["start"],
                    }
                )
            )
        previous_speaker = speaker
    return markers


def _report_counts(
    sentences: Sequence[Mapping[str, Any]],
    descriptions: Sequence[Mapping[str, Any]],
    markers: Sequence[Mapping[str, Any]],
    affected: Sequence[str],
    reused: Sequence[str],
    recomputed: Sequence[str],
    recovered: Sequence[str],
) -> dict[str, int]:
    return {
        "affected": len(affected),
        "descriptions": len(descriptions),
        "inserted_descriptions": sum(
            1 for item in descriptions if str(item["status"]).startswith("inserted")
        ),
        "recomputed": len(recomputed),
        "recovered": len(recovered),
        "reused": len(reused),
        "sentences": len(sentences),
        "speaker_markers": len(markers),
    }


def compute_layout(
    asset: SubtitleAsset,
    wav: WavInfo,
    frames: FrameTable,
    *,
    params: LayoutParams | None = None,
    revision: int = 1,
    parent_hash: str = ZERO_HASH,
    previous_report: Mapping[str, Any] | None = None,
    affected_ids: Sequence[str] | None = None,
    recovered_entries: Mapping[str, Mapping[str, Any]] | None = None,
    checkpoint: CheckpointCallback | None = None,
    cancel_requested: Callable[[], bool] | None = None,
) -> dict[str, Any]:
    """Compute a complete immutable layout report.

    ``previous_report`` is a cache.  Sentences outside the dynamically expanded
    affected set are copied byte-for-byte and retain their entry hash.  The
    function has no clock, random source or mutable module state.
    """

    params = params or asset.parameters
    timings_list = validate_media_and_asset(asset, wav, frames, params)
    timing_by_id = {item.sentence_id: item for item in timings_list}
    sentences = tuple(sorted(asset.sentences, key=lambda item: (
        timing_by_id[item.id].orig_start_sample,
        item.id,
    )))
    sentence_order = [item.id for item in sentences]
    descriptions_by_anchor = _normalise_descriptions_by_anchor(asset.descriptions, sentence_order)

    previous_entries: dict[str, Mapping[str, Any]] = {}
    if previous_report:
        for item in previous_report.get("sentences", []):
            if isinstance(item, Mapping) and isinstance(item.get("sentence_id"), str):
                previous_entries[str(item["sentence_id"])] = item

    if not previous_report:
        affected: set[str] = set(sentence_order)
    elif affected_ids is None:
        affected = set(previous_entries)
    else:
        affected = {sentence_id for sentence_id in affected_ids if sentence_id in timing_by_id}
        affected.update(set(sentence_order) - set(previous_entries))
        for removed in set(previous_entries) - set(sentence_order):
            # Removals can move later items, so a full suffix is affected.
            removed_timing = None
            for old_entry in previous_entries.values():
                if old_entry.get("sentence_id") == removed:
                    removed_timing = int(old_entry["orig_start_sample"])
            if removed_timing is None:
                affected.update(sentence_order)
            else:
                affected.update(
                    sentence_id
                    for sentence_id in sentence_order
                    if timing_by_id[sentence_id].orig_start_sample >= removed_timing
                )
    recovered = {
        sentence_id: dict(entry)
        for sentence_id, entry in (recovered_entries or {}).items()
        if sentence_id in timing_by_id
    }

    min_duration_samples = ms_to_samples(params.min_duration_ms)
    min_gap_samples = ms_to_samples(params.min_gap_ms)
    actual_recomputed: list[str] = []
    actual_reused: list[str] = []
    actual_recovered: list[str] = []
    final_entries: list[dict[str, Any]] = []
    descriptions_out: list[dict[str, Any]] = []
    cut_ids: set[str] = set()
    sentence_trims: dict[str, int] = {}
    description_cut_ids: set[str] = set()
    previous_item_end: int | None = None

    def cancelled() -> None:
        if cancel_requested is not None and cancel_requested():
            raise CancelledError(
                "layout cancelled at a sentence checkpoint",
                {"processed": len(actual_recomputed) + len(actual_reused)},
            )

    for sentence in sentences:
        cancelled()
        timing = timing_by_id[sentence.id]
        old_entry = previous_entries.get(sentence.id)
        fingerprint = _sentence_fingerprint(sentence, params, timing)
        recoverable = recovered.get(sentence.id)
        use_entry: dict[str, Any] | None = None
        was_recovered = False

        if (
            sentence.id not in affected
            and old_entry is not None
            and _entry_matches_sentence(old_entry, sentence, params, timing)
        ):
            earliest = max(
                timing.orig_start_sample,
                (previous_item_end + min_gap_samples) if previous_item_end is not None else 0,
            )
            if int(old_entry["start_sample"]) >= earliest:
                use_entry = dict(old_entry)
                actual_reused.append(sentence.id)

        if use_entry is None and recoverable is not None:
            if recoverable.get("input_fingerprint") == fingerprint and _entry_matches_sentence(
                recoverable, sentence, params, timing
            ):
                use_entry = dict(recoverable)
                was_recovered = True
                actual_reused.append(sentence.id)
                actual_recovered.append(sentence.id)

        if use_entry is None:
            desired = duration_for_text(sentence.text, effective_cps(sentence, params))
            earliest = max(
                timing.orig_start_sample,
                (previous_item_end + min_gap_samples) if previous_item_end is not None else 0,
            )
            duration = max(desired, min_duration_samples, 1)
            clipped = False
            media_end = wav.frame_count
            if earliest + duration > media_end:
                available = media_end - earliest
                if available <= 0:
                    raise BusinessError(
                        E_TIMEBASE_INVALID,
                        "layout cannot fit the sentence before the end of the audio",
                        {
                            "sentence_id": sentence.id,
                            "field": "end",
                            "require_samples": duration,
                            "available_samples": max(0, available),
                        },
                    )
                duration = available
                clipped = True
            entry = _make_sentence_entry(
                sentence,
                timing,
                params,
                start=earliest,
                duration=duration,
                desired=desired,
                min_duration_samples=min_duration_samples,
                clipped=clipped,
                description_cut=sentence.id in description_cut_ids,
            )
            use_entry = _entry_with_frames(entry, frames)
            affected.add(sentence.id)
            actual_recomputed.append(sentence.id)
        else:
            # Cached entries are immutable.  Re-frame them from the committed
            # start/end so a frame table change cannot leave stale boundaries.
            use_entry = _entry_with_frames(use_entry, frames)
            if sentence.id in description_cut_ids:
                # A description in this version has changed the original
                # placement.  Recomputing is required even if the numeric slot
                # happens to match the cached one.
                desired = duration_for_text(sentence.text, effective_cps(sentence, params))
                earliest = max(
                    timing.orig_start_sample,
                    (previous_item_end + min_gap_samples) if previous_item_end is not None else 0,
                )
                duration = max(desired, min_duration_samples, 1)
                clipped = earliest + duration > wav.frame_count
                if clipped:
                    duration = max(1, wav.frame_count - earliest)
                use_entry = _entry_with_frames(
                    _make_sentence_entry(
                        sentence,
                        timing,
                        params,
                        start=earliest,
                        duration=duration,
                        desired=desired,
                        min_duration_samples=min_duration_samples,
                        clipped=clipped,
                        description_cut=True,
                    ),
                    frames,
                )
                affected.add(sentence.id)
                actual_recomputed.append(sentence.id)
                if sentence.id in actual_reused:
                    actual_reused.remove(sentence.id)
                if sentence.id in actual_recovered:
                    actual_recovered.remove(sentence.id)

        final_entries.append(use_entry)
        previous_item_end = int(use_entry["end_sample"])

        next_sentence = _next_sentence_input(sentences, sentence.id)
        next_orig_start = timing_by_id[next_sentence.id].orig_start_sample if next_sentence else wav.frame_count

        for description in descriptions_by_anchor[sentence.id]:
            cancelled()
            allocation_start = previous_item_end + min_gap_samples
            requested, minimum = _description_duration(description)
            available_before_next = max(0, next_orig_start - allocation_start - min_gap_samples)
            media_available = max(0, wav.frame_count - allocation_start)
            hard_available = min(available_before_next, media_available) if next_sentence else media_available
            cut_next = False
            clipped = False
            if requested <= hard_available:
                allocated = requested
                status = "inserted"
            elif description.priority > params.sentence_priority and media_available >= requested:
                allocated = requested
                status = "inserted_cut"
                cut_next = next_sentence is not None
            elif hard_available >= minimum:
                allocated = hard_available
                status = "inserted_compressed"
            elif (
                description.priority == params.sentence_priority
                and next_sentence is not None
                and hard_available > 0
            ):
                raise BusinessError(
                    E_DESC_CONFLICT,
                    "description conflict cannot be resolved at equal priority",
                    {
                        "description_id": description.id,
                        "sentence_id": next_sentence.id,
                        "available_samples": hard_available,
                        "minimum_samples": minimum,
                        "priority": description.priority,
                    },
                )
            else:
                allocated = 0
                status = "dropped"

            if allocated and allocation_start + allocated > wav.frame_count:
                allocated = max(0, wav.frame_count - allocation_start)
                clipped = True
                if allocated < minimum:
                    allocated = 0
                    status = "dropped"
                elif status == "inserted":
                    status = "inserted_compressed"

            end = allocation_start + allocated
            cut_actions: list[dict[str, Any]] = []
            if cut_next and next_sentence is not None and allocated:
                cut_ids.add(next_sentence.id)
                description_cut_ids.add(next_sentence.id)
                cut_amount = max(0, end + min_gap_samples - next_orig_start)
                if cut_amount:
                    sentence_trims[next_sentence.id] = max(
                        sentence_trims.get(next_sentence.id, 0), cut_amount
                    )
                cut_actions.append(
                    {
                        "cut_samples": cut_amount,
                        "cut_start_sample": next_orig_start,
                        "dropped": False,
                        "sentence_id": next_sentence.id,
                    }
                )
                affected.add(next_sentence.id)
            description_entry = _finalize_description(
                {
                    "after_sentence_id": sentence.id,
                    "allocated_duration_samples": allocated,
                    "cut_actions": cut_actions,
                    "end": format_timecode(samples_to_ticks(end)),
                    "end_sample": end,
                    "id": description.id,
                    "max_cps": description.max_cps,
                    "priority": description.priority,
                    "requested_duration_samples": requested,
                    "speaker": description.speaker,
                    "start": format_timecode(samples_to_ticks(allocation_start)),
                    "start_sample": allocation_start,
                    "status": status,
                    "text": description.text,
                }
            )
            descriptions_out.append(description_entry)
            if allocated:
                previous_item_end = end
            # A dropped/compressed description does not occupy a slot beyond
            # what was allocated.

        next_index = sentence_order.index(sentence.id) + 1
        if next_index < len(sentence_order):
            next_id = sentence_order[next_index]
            next_timing = timing_by_id[next_id]
            next_earliest = max(
                next_timing.orig_start_sample,
                previous_item_end + min_gap_samples,
            )
            old_next = previous_entries.get(next_id)
            # The offline pass always places the next sentence at this exact
            # earliest slot.  If the cached slot differs, incremental layout
            # could otherwise preserve a start that the offline pass would move
            # earlier (or later).  This is the dependency-closure step.
            if old_next is None or int(old_next["start_sample"]) != next_earliest:
                affected.add(next_id)
        if checkpoint is not None:
            checkpoint(
                Checkpoint(
                    sentence_id=sentence.id,
                    entry=dict(use_entry),
                    affected_ids=tuple(sorted(affected)),
                    reused_ids=tuple(sorted(actual_reused)),
                )
            )
        cancelled()

    markers = _build_speaker_markers(final_entries)
    affected_list = sorted_unique(affected)
    reused_list = sorted_unique(actual_reused)
    recomputed_list = sorted_unique(actual_recomputed)
    recovered_list = sorted_unique(actual_recovered)
    report: dict[str, Any] = {
        "affected_sentence_ids": affected_list,
        "asset_id": asset.asset_id,
        "counts": _report_counts(
            final_entries,
            descriptions_out,
            markers,
            affected_list,
            reused_list,
            recomputed_list,
            recovered_list,
        ),
        "cut_sentence_ids": sorted_unique(cut_ids),
        "descriptions": descriptions_out,
        "params": params.to_dict(),
        "parent_hash": parent_hash,
        "recomputed_sentence_ids": recomputed_list,
        "recovered_sentence_ids": recovered_list,
        "reused_sentence_ids": reused_list,
        "revision": revision,
        "schema": SCHEMA_REPORT,
        "sentence_trims": dict(sorted(sentence_trims.items())),
        "sentences": final_entries,
        "speaker_markers": markers,
    }
    report["commit_hash"] = hash_without(report, "commit_hash")
    return report


def _entry_signature(entry: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "end_sample": entry.get("end_sample"),
        "entry_hash": entry.get("entry_hash"),
        "frame_end": entry.get("frame_end"),
        "frame_start": entry.get("frame_start"),
        "sentence_id": entry.get("sentence_id"),
        "start_sample": entry.get("start_sample"),
    }


def compare_layouts(
    online: Mapping[str, Any],
    offline: Mapping[str, Any],
    *,
    sync_markers: Sequence[str] | None = None,
) -> list[dict[str, Any]]:
    online_by_id = {
        str(item["sentence_id"]): item for item in online.get("sentences", [])
    }
    offline_by_id = {
        str(item["sentence_id"]): item for item in offline.get("sentences", [])
    }
    if sync_markers is None:
        selected = sorted(set(online_by_id) | set(offline_by_id))
    else:
        selected = list(dict.fromkeys(sync_markers))
    mismatches: list[dict[str, Any]] = []
    for sentence_id in selected:
        left = online_by_id.get(sentence_id)
        right = offline_by_id.get(sentence_id)
        left_sig = _entry_signature(left) if left else None
        right_sig = _entry_signature(right) if right else None
        if left_sig != right_sig:
            mismatches.append(
                {
                    "online": left_sig,
                    "offline": right_sig,
                    "sentence_id": sentence_id,
                }
            )
    if mismatches:
        raise BusinessError(
            E_LAYOUT_MISMATCH,
            "online preview and offline layout differ at synchronisation markers",
            {
                "mismatch_count": len(mismatches),
                "mismatches": mismatches,
                "sync_markers": selected,
            },
        )
    return []


def compute_affected_sentence_ids(
    *,
    old_asset: SubtitleAsset | None,
    new_asset: SubtitleAsset,
    old_params: LayoutParams | None,
    new_params: LayoutParams,
    previous_report: Mapping[str, Any] | None,
    old_timings: Mapping[str, SentenceTiming] | None = None,
) -> list[str]:
    order = sorted(
        new_asset.sentences,
        key=lambda item: (
            parse_timecode(item.start, sentence_id=item.id, field="start"),
            item.id,
        ),
    )
    all_ids = [item.id for item in order]
    if old_asset is None or previous_report is None or old_params is None:
        return all_ids
    old_map = {item.id: item for item in old_asset.sentences}
    new_map = {item.id: item for item in new_asset.sentences}
    old_order = sorted(
        old_asset.sentences,
        key=lambda item: (parse_timecode(item.start, sentence_id=item.id, field="start"), item.id),
    )
    old_order_ids = [item.id for item in old_order]
    affected: set[str] = set()

    if old_order_ids != all_ids:
        # Added/removed/reordered sentences can move their entire suffix.
        common_prefix = 0
        for left, right in zip(old_order_ids, all_ids):
            if left != right:
                break
            common_prefix += 1
        affected.update(all_ids[common_prefix:])
        affected.update(set(old_order_ids[common_prefix:]) & set(all_ids))
        affected.update(set(old_map) - set(new_map))

    for sentence in order:
        old_sentence = old_map.get(sentence.id)
        if old_sentence is None or old_sentence.to_dict() != sentence.to_dict():
            affected.add(sentence.id)
            if old_sentence is not None and old_sentence.speaker != sentence.speaker:
                next_sentence = _next_sentence_input(tuple(order), sentence.id)
                if next_sentence is not None:
                    affected.add(next_sentence.id)

    old_report_entries = {
        str(item["sentence_id"]): item
        for item in (previous_report.get("sentences", []) if previous_report else [])
    }
    if old_params.default_cps != new_params.default_cps:
        affected.update(sentence.id for sentence in order if sentence.cps is None)
    if old_params.min_duration_ms != new_params.min_duration_ms:
        old_min = ms_to_samples(old_params.min_duration_ms)
        new_min = ms_to_samples(new_params.min_duration_ms)
        for sentence in order:
            desired = duration_for_text(sentence.text, effective_cps(sentence, new_params))
            if max(desired, old_min) != max(desired, new_min):
                affected.add(sentence.id)
    if old_params.min_gap_ms != new_params.min_gap_ms:
        old_gap = ms_to_samples(old_params.min_gap_ms)
        new_gap = ms_to_samples(new_params.min_gap_ms)
        previous_end: int | None = None
        for sentence in order:
            entry = old_report_entries.get(sentence.id)
            if previous_end is not None and entry is not None and old_gap != new_gap:
                original_start = int(entry["orig_start_sample"])
                if int(entry["start_sample"]) > original_start or original_start - previous_end < max(old_gap, new_gap):
                    affected.add(sentence.id)
            if entry is not None:
                previous_end = int(entry["end_sample"])
        # Descriptions are placed in gaps, so their anchors must be revisited.
        described_anchors = {item.after_sentence_id for item in new_asset.descriptions}
        affected.update(described_anchors)
    if old_params.sentence_priority != new_params.sentence_priority:
        affected.update(all_ids)
    if (
        old_params.default_channel != new_params.default_channel
        or dict(old_params.channel_map) != dict(new_params.channel_map)
    ):
        for sentence in order:
            old_channel = old_params.channel_map.get(
                old_map.get(sentence.id, sentence).speaker, old_params.default_channel
            )
            new_channel = new_params.channel_map.get(sentence.speaker, new_params.default_channel)
            if old_channel != new_channel:
                affected.add(sentence.id)
    old_desc = {item.id: item for item in old_asset.descriptions}
    new_desc = {item.id: item for item in new_asset.descriptions}
    if old_desc != new_desc:
        all_anchors = {
            item.after_sentence_id for item in old_asset.descriptions
        } | {item.after_sentence_id for item in new_asset.descriptions}
        for sentence in order:
            if sentence.id in all_anchors:
                affected.add(sentence.id)
                next_sentence = _next_sentence_input(tuple(order), sentence.id)
                if next_sentence is not None:
                    affected.add(next_sentence.id)
    for entry in old_report_entries.values():
        sentence_id = str(entry["sentence_id"])
        if sentence_id not in new_map:
            affected.update(
                item.id
                for item in order
                if parse_timecode(item.start, sentence_id=item.id, field="start")
                >= int(entry["orig_start_sample"]) * 125
            )
    return sorted(affected)

