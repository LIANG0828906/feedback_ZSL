"""Project loading and structural validation."""

import json
import os

import numpy as np

from .errors import E_DECODE, E_REF, E_SCHEMA, E_WIN, PipelineError
from .hashing import content_hash
from .model import AudioSegment, PresentationParams, Project, Subtitle

REQUIRED_MANIFEST_FIELDS = (
    "project_id", "sample_rate", "frame_length", "frame_shift",
    "energy_threshold", "segments", "subtitles", "params",
)
REQUIRED_SEGMENT_FIELDS = ("segment_id", "start_sample", "end_sample")
REQUIRED_SUBTITLE_FIELDS = ("subtitle_id", "text", "segment_ref")
REQUIRED_PARAM_FIELDS = (
    "line_height_px", "max_chars_per_line", "vertical_margin_px",
    "screen_height_px",
)


def _read_json(path):
    try:
        with open(path, encoding="utf-8") as fh:
            return json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise PipelineError(E_DECODE, f"cannot decode {os.path.basename(path)}: {exc}")


def _require_fields(obj, fields, subtitle_id=None):
    missing = [name for name in fields if name not in obj or obj[name] is None]
    if missing:
        raise PipelineError(
            E_SCHEMA, f"missing required field(s): {', '.join(missing)}",
            subtitle_id=subtitle_id, field=missing[0])


def _is_positive_int(value):
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def load_project(project_dir):
    manifest_path = os.path.join(project_dir, "manifest.json")
    manifest = _read_json(manifest_path)
    _require_fields(manifest, REQUIRED_MANIFEST_FIELDS)

    for name in ("sample_rate", "frame_length", "frame_shift"):
        if not _is_positive_int(manifest[name]):
            raise PipelineError(E_SCHEMA, f"{name} must be a positive integer",
                                field=name)
    if not isinstance(manifest["energy_threshold"], (int, float)) or \
            manifest["energy_threshold"] <= 0:
        raise PipelineError(E_SCHEMA, "energy_threshold must be positive",
                            field="energy_threshold")

    segments = _parse_segments(manifest["segments"])
    segment_map = {seg.segment_id: seg for seg in segments}
    subtitles = _parse_subtitles(manifest["subtitles"], segment_map)
    params = _parse_params(manifest["params"])

    fingerprint_path = os.path.join(project_dir, "fingerprint.txt")
    fingerprint = None
    if os.path.exists(fingerprint_path):
        with open(fingerprint_path, encoding="utf-8") as fh:
            fingerprint = fh.read().strip()
    manifest_body = {k: v for k, v in manifest.items()}
    computed = content_hash(manifest_body)

    wave_path = os.path.join(project_dir, "waveform.npy")
    waveform = None
    if os.path.exists(wave_path):
        try:
            waveform = np.load(wave_path)
        except (OSError, ValueError) as exc:
            raise PipelineError(E_DECODE, f"corrupt waveform: {exc}")

    project = Project(
        project_id=manifest["project_id"],
        sample_rate=manifest["sample_rate"],
        frame_length=manifest["frame_length"],
        frame_shift=manifest["frame_shift"],
        energy_threshold=manifest["energy_threshold"],
        segments=tuple(segments),
        subtitles=tuple(subtitles),
        params=params,
        manifest_fingerprint=fingerprint or computed,
    )
    return project, manifest, waveform, computed


def _parse_segments(raw):
    if not isinstance(raw, list) or not raw:
        raise PipelineError(E_SCHEMA, "segments must be a non-empty list",
                            field="segments")
    segments = []
    seen = set()
    for item in raw:
        _require_fields(item, REQUIRED_SEGMENT_FIELDS)
        if not _is_positive_int(item["start_sample"]) or \
                not _is_positive_int(item["end_sample"]):
            raise PipelineError(E_SCHEMA, "segment bounds must be positive ints",
                                field="start_sample")
        if item["end_sample"] <= item["start_sample"]:
            raise PipelineError(E_SCHEMA, "segment end must exceed start",
                                field="end_sample")
        if item["segment_id"] in seen:
            raise PipelineError(E_SCHEMA,
                                f"duplicate segment id {item['segment_id']}",
                                field="segment_id")
        seen.add(item["segment_id"])
        segments.append(AudioSegment(**item))
    segments.sort(key=lambda seg: seg.start_sample)
    return segments


def _parse_subtitles(raw, segment_map):
    if not isinstance(raw, list) or not raw:
        raise PipelineError(E_SCHEMA, "subtitles must be a non-empty list",
                            field="subtitles")
    subtitles = []
    seen_ids = set()
    for item in raw:
        _require_fields(item, REQUIRED_SUBTITLE_FIELDS,
                        subtitle_id=item.get("subtitle_id"))
        sid = item["subtitle_id"]
        if not isinstance(sid, int) or isinstance(sid, bool) or sid <= 0:
            raise PipelineError(E_SCHEMA, "subtitle_id must be a positive int",
                                subtitle_id=sid, field="subtitle_id")
        if sid in seen_ids:
            raise PipelineError(E_SCHEMA, f"duplicate subtitle_id {sid}",
                                subtitle_id=sid, field="subtitle_id")
        seen_ids.add(sid)
        if not isinstance(item["text"], str) or not item["text"].strip():
            raise PipelineError(E_SCHEMA, "text must be a non-empty string",
                                subtitle_id=sid, field="text")

        ref = item["segment_ref"]
        if ref not in segment_map:
            raise PipelineError(E_REF,
                                f"segment_ref '{ref}' does not exist",
                                subtitle_id=sid, field="segment_ref")
        seg = segment_map[ref]

        explicit_start = item.get("explicit_start")
        explicit_end = item.get("explicit_end")
        if (explicit_start is None) != (explicit_end is None):
            raise PipelineError(E_SCHEMA,
                                "explicit window needs both start and end",
                                subtitle_id=sid, field="explicit_start")
        if explicit_start is not None:
            if not isinstance(explicit_start, int) or not isinstance(explicit_end, int):
                raise PipelineError(E_SCHEMA,
                                    "explicit window bounds must be ints",
                                    subtitle_id=sid, field="explicit_start")
            if explicit_end <= explicit_start:
                raise PipelineError(E_WIN, "explicit window end <= start",
                                    subtitle_id=sid, field="explicit_end")
            # Window must stay inside the referenced segment (no cross-segment).
            if explicit_start < seg.start_sample or explicit_end > seg.end_sample:
                raise PipelineError(
                    E_WIN,
                    f"explicit window [{explicit_start},{explicit_end}) exceeds "
                    f"segment [{seg.start_sample},{seg.end_sample})",
                    subtitle_id=sid, field="explicit_start")

        speed = item.get("max_read_speed")
        if speed is not None and (not isinstance(speed, (int, float))
                                  or isinstance(speed, bool) or speed <= 0):
            raise PipelineError(E_SCHEMA, "max_read_speed must be positive",
                                subtitle_id=sid, field="max_read_speed")

        subtitles.append(Subtitle(
            subtitle_id=sid, text=item["text"], segment_ref=ref,
            explicit_start=explicit_start, explicit_end=explicit_end,
            max_read_speed=speed,
        ))
    subtitles.sort(key=lambda sub: sub.subtitle_id)
    return subtitles


def _parse_params(raw):
    _require_fields(raw, REQUIRED_PARAM_FIELDS)
    for name in ("line_height_px", "vertical_margin_px", "screen_height_px"):
        if not _is_positive_int(raw[name]):
            raise PipelineError(E_SCHEMA,
                                f"params.{name} must be a positive integer",
                                field=name)
    # Width of 0 is schema-valid; the wrapping stage raises E_PARA so the
    # error is attributed to the layout constraint, not the manifest shape.
    width = raw["max_chars_per_line"]
    if not isinstance(width, int) or isinstance(width, bool) or width < 0:
        raise PipelineError(E_SCHEMA,
                            "params.max_chars_per_line must be a non-negative int",
                            field="max_chars_per_line")
    kwargs = {
        "line_height_px": raw["line_height_px"],
        "max_chars_per_line": raw["max_chars_per_line"],
        "vertical_margin_px": raw["vertical_margin_px"],
        "screen_height_px": raw["screen_height_px"],
    }
    optional = (
        "wrap_weight_raggedness", "wrap_weight_break", "min_line_dwell_s",
        "line_dwell_s", "screen_gap_s", "default_read_speed",
    )
    for name in optional:
        if name in raw:
            if not isinstance(raw[name], (int, float)) or raw[name] <= 0:
                raise PipelineError(E_SCHEMA, f"params.{name} must be positive",
                                    field=name)
            kwargs[name] = raw[name]
    if "tradeoff_priority" in raw:
        priority = tuple(raw["tradeoff_priority"])
        allowed = {"compress_margin", "reclaim_gap", "add_screens"}
        if set(priority) != allowed or len(priority) != 3:
            raise PipelineError(E_SCHEMA,
                                "tradeoff_priority must permute the 3 actions",
                                field="tradeoff_priority")
        kwargs["tradeoff_priority"] = priority
    return PresentationParams(**kwargs)


def load_ground_truth(project_dir):
    path = os.path.join(project_dir, "ground_truth.json")
    if not os.path.exists(path):
        return None
    return _read_json(path)
