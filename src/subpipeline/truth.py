"""Analytic ground-truth generation for deterministic samples."""

from .timing import extension_caps, resolve_subtitle
from .wrapping import layout_text


def build_ground_truth(project):
    """Compute the expected layout/timing truth for a whole project."""
    segment_map = project.segment_index()
    bounds = extension_caps(project.segments)
    entries = []
    for subtitle in project.subtitles:
        outcome = resolve_subtitle(
            subtitle, segment_map[subtitle.segment_ref],
            project.params, bounds, project.sample_rate)
        screens = outcome["screens"]
        entries.append({
            "subtitle_id": subtitle.subtitle_id,
            "segment_ref": subtitle.segment_ref,
            "start_sample": outcome["start_sample"],
            "end_sample": outcome["end_sample"],
            "screen_count": len(screens),
            "line_counts": [s.line_count for s in screens],
            "line_char_counts": [[len(line) for line in s.lines]
                                 for s in screens],
            "lines": [list(s.lines) for s in screens],
            "required_duration_s": round(outcome["required_duration_s"], 6),
            "actual_duration_s": round(outcome["actual_duration_s"], 6),
            "read_speed_cps": outcome["read_speed_cps"],
            "tradeoffs": [t.to_dict() for t in outcome["tradeoffs"]],
        })
    starts = [e["start_sample"] for e in entries]
    ends = [e["end_sample"] for e in entries]
    total_samples = max(ends) - min(starts)
    return {
        "project_id": project.project_id,
        "sample_rate": project.sample_rate,
        "entries": entries,
        "total_occupied_samples": total_samples,
        "total_occupied_s": round(total_samples / project.sample_rate, 6),
    }


def expected_wrap(project, text):
    """Helper for unit tests: expected lines for a text under project params."""
    screens = layout_text(text, project.params)
    return [list(s.lines) for s in screens]
