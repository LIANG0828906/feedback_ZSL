"""Readable-duration model, window extension and trade-off arbitration."""

from .errors import E_GAP, E_TIMING, PipelineError
from .wrapping import layout_text, reflow_with_screens


def readable_duration_s(screens, params, read_speed_cps, margin_scale=1.0):
    """Minimum readable duration for a laid-out subtitle (seconds).

    Model (documented in docs/DESIGN.md):
      read time   = total characters / read speed cap
      line dwell  = (min dwell + per-line dwell * max lines on one screen)
                    * margin scale
      screen gaps = screen_gap * (screen count - 1)
    More screens lower the max-lines-per-screen dwell term, while margin
    compression scales the dwell directly.
    """
    chars = sum(screen.char_count for screen in screens)
    max_lines = max(screen.line_count for screen in screens)
    read_time = chars / read_speed_cps
    dwell = (params.min_line_dwell_s
             + params.line_dwell_s * max_lines) * margin_scale
    gaps = params.screen_gap_s * (len(screens) - 1)
    return read_time + dwell + gaps


def extension_caps(segments):
    """Per-segment [earliest, latest] extension bounds in samples.

    Gaps are shared symmetrically: each side may claim at most half of the
    gap, which guarantees neighbours can never extend into one another.
    """
    ordered = sorted(segments, key=lambda seg: seg.start_sample)
    bounds = {}
    for pos, seg in enumerate(ordered):
        if pos == 0:
            earliest = 0
        else:
            prev = ordered[pos - 1]
            earliest = prev.end_sample + (seg.start_sample - prev.end_sample) // 2
        if pos == len(ordered) - 1:
            latest = None
        else:
            nxt = ordered[pos + 1]
            latest = seg.end_sample + (nxt.start_sample - seg.end_sample) // 2
        bounds[seg.segment_id] = (earliest, latest)
    return bounds


def derive_base_window(subtitle, segment):
    if subtitle.explicit_start is not None:
        return subtitle.explicit_start, subtitle.explicit_end
    return segment.start_sample, segment.end_sample


def extend_window(start, end, required_end, earliest, latest, fixed_left=False):
    """Grow [start, end) until end >= required_end while inside the bounds.

    Returns (new_start, new_end, shortage_samples).
    """
    needed = required_end - end
    if needed <= 0:
        return start, end, 0
    new_start, new_end = start, end
    if fixed_left:
        if latest is None:
            return new_start, new_end + needed, 0
        take = min(needed, latest - new_end)
        new_end += take
        return new_start, new_end, needed - take
    left_room = new_start - earliest
    take_left = min(needed // 2, left_room)
    new_start -= take_left
    needed -= take_left
    if latest is None:
        return new_start, new_end + needed, 0
    take_right = min(needed, latest - new_end)
    new_end += take_right
    needed -= take_right
    if needed > 0:
        more_left = min(needed, new_start - earliest)
        new_start -= more_left
        needed -= more_left
    return new_start, new_end, needed


def window_seconds(window, sample_rate):
    return (window[1] - window[0]) / sample_rate


def layout_subtitle(subtitle, params, margin_scale=1.0, force_screens=0):
    """Run wrapping + timing; return (screens, required_s, read_speed)."""
    read_speed = subtitle.max_read_speed or params.default_read_speed
    if force_screens > 0:
        screens = reflow_with_screens(subtitle.text, params, force_screens)
    else:
        screens = layout_text(subtitle.text, params)
    required_s = readable_duration_s(screens, params, read_speed, margin_scale)
    return screens, required_s, read_speed


def _action_available(action, margin_scale, extra_screens, max_extra,
                      start, end, earliest, latest):
    if action == "compress_margin":
        return margin_scale > 0.5
    if action == "add_screens":
        return extra_screens < max_extra
    if start > earliest:
        return True
    return latest is not None and end < latest


def resolve_subtitle(subtitle, segment, params, bounds, sample_rate):
    """Deterministic two-stage resolution for one subtitle."""
    earliest, latest = bounds[segment.segment_id]
    base_start, base_end = derive_base_window(subtitle, segment)
    fixed_left = subtitle.explicit_start is not None
    tradeoffs = []
    margin_scale = 1.0
    extra_screens = 0

    screens, required_s, read_speed = layout_subtitle(subtitle, params)

    left_room = base_start - earliest
    right_room = None if latest is None else latest - base_end
    total_room = None if latest is None else left_room + right_room

    # Stage A (requirement 2): extend into neighbouring silence gaps.
    required_end = base_start + int(_round_half_up(required_s * sample_rate))
    start, end, shortage = extend_window(
        base_start, base_end, required_end, earliest, latest, fixed_left)

    total_extension_room = (base_start - earliest) + (
        0 if latest is None else max(0, latest - base_end))
    if shortage > 0 and total_extension_room == 0:
        actual_s = window_seconds((start, end), sample_rate)
        raise PipelineError(
            E_GAP,
            f"silence gap too small; window {actual_s:.3f}s cannot reach "
            f"required {required_s:.3f}s without overlapping another segment",
            subtitle_id=subtitle.subtitle_id,
            details={"required_s": required_s, "available_s": actual_s,
                     "start_sample": start, "end_sample": end})

    # Stage B (requirement 4): priority-ordered trade-offs.
    order = 0
    max_extra = max(0, sum(s.line_count for s in screens) - 1)
    while end - start < int(_round_half_up(required_s * sample_rate)):
        action = next((a for a in params.tradeoff_priority
                       if _action_available(a, margin_scale, extra_screens,
                                            max_extra, start, end, earliest,
                                            latest)), None)
        if action is None:
            break
        before_required = required_s
        order += 1
        if action == "compress_margin":
            margin_scale = 0.5
            reason = "halve vertical margin dwell to shrink readable duration"
            screens, required_s, read_speed = layout_subtitle(
                subtitle, params, margin_scale=margin_scale,
                force_screens=extra_screens)
        elif action == "add_screens":
            extra_screens += 1
            reason = "add an extra screen to reduce max lines per screen"
            screens, required_s, read_speed = layout_subtitle(
                subtitle, params, margin_scale=margin_scale,
                force_screens=extra_screens)
        else:
            reason = "reclaim remaining neighbouring silence gap"
        target_end = start + int(_round_half_up(required_s * sample_rate))
        start, end, _short = extend_window(
            start, end, target_end, earliest, latest, fixed_left)
        tradeoffs.append(TradeOff(
            order, action, reason, before_required, required_s,
            window_seconds((start, end), sample_rate)))

    final_required = int(_round_half_up(required_s * sample_rate))
    if end - start < final_required:
        actual_s = window_seconds((start, end), sample_rate)
        raise PipelineError(
            E_TIMING,
            f"window {actual_s:.3f}s shorter than required {required_s:.3f}s "
            "after all trade-offs",
            subtitle_id=subtitle.subtitle_id,
            details={"actual_s": actual_s, "required_s": required_s,
                     "start_sample": start, "end_sample": end})

    return {
        "start_sample": start,
        "end_sample": end,
        "screens": screens,
        "required_duration_s": required_s,
        "actual_duration_s": window_seconds((start, end), sample_rate),
        "tradeoffs": tuple(t.as_record() for t in tradeoffs),
        "read_speed_cps": read_speed,
        "margin_scale": margin_scale,
        "extra_screens": extra_screens,
    }


class TradeOff:
    def __init__(self, order, action, reason, before, after, window_s):
        self.order = order
        self.action = action
        self.reason = reason
        self.before_required_s = before
        self.after_required_s = after
        self.window_s = window_s

    def as_record(self):
        from .model import TradeOffRecord
        return TradeOffRecord(
            order=self.order, action=self.action, reason=self.reason,
            before_required_s=round(self.before_required_s, 6),
            after_required_s=round(self.after_required_s, 6),
            window_s=round(self.window_s, 6))


def _round_half_up(value):
    import math
    return math.floor(value + 0.5)
