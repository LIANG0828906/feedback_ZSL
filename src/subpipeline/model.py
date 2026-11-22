"""Typed data models shared by every pipeline stage."""

from dataclasses import dataclass, field, asdict


@dataclass(frozen=True)
class AudioSegment:
    segment_id: str
    start_sample: int
    end_sample: int

    def to_dict(self):
        return asdict(self)


@dataclass(frozen=True)
class Subtitle:
    subtitle_id: int          # 1-based dialogue number
    text: str
    segment_ref: str
    explicit_start: int | None = None
    explicit_end: int | None = None
    max_read_speed: float | None = None   # characters per second; None -> default

    def to_dict(self):
        return asdict(self)


@dataclass(frozen=True)
class PresentationParams:
    line_height_px: int
    max_chars_per_line: int
    vertical_margin_px: int
    screen_height_px: int
    wrap_weight_raggedness: float = 1.0
    wrap_weight_break: float = 0.5
    # Timing model (see docs/DESIGN.md for the readable-duration formula).
    min_line_dwell_s: float = 0.35
    line_dwell_s: float = 0.25
    screen_gap_s: float = 0.06
    # Trade-off priority: margin compression -> gap reclaim -> extra screens.
    tradeoff_priority: tuple = ("compress_margin", "reclaim_gap", "add_screens")
    default_read_speed: float = 12.0

    @property
    def max_lines_per_screen(self):
        usable = self.screen_height_px - 2 * self.vertical_margin_px
        return max(1, usable // self.line_height_px)

    def to_dict(self):
        data = asdict(self)
        data["tradeoff_priority"] = list(self.tradeoff_priority)
        data["max_lines_per_screen"] = self.max_lines_per_screen
        return data


@dataclass(frozen=True)
class Project:
    project_id: str
    sample_rate: int
    frame_length: int
    frame_shift: int
    energy_threshold: float
    segments: tuple
    subtitles: tuple
    params: PresentationParams
    manifest_fingerprint: str

    def segment_index(self):
        return {seg.segment_id: seg for seg in self.segments}


@dataclass(frozen=True)
class Screen:
    lines: tuple              # tuple[str]

    @property
    def line_count(self):
        return len(self.lines)

    @property
    def char_count(self):
        return sum(len(line) for line in self.lines)

    def to_dict(self):
        return {"lines": list(self.lines)}


@dataclass(frozen=True)
class TradeOffRecord:
    order: int
    action: str               # compress_margin | reclaim_gap | add_screens
    reason: str
    before_required_s: float
    after_required_s: float
    window_s: float

    def to_dict(self):
        return asdict(self)


@dataclass(frozen=True)
class SubtitleResult:
    subtitle_id: int
    segment_ref: str
    start_sample: int
    end_sample: int
    screens: tuple            # tuple[Screen]
    required_duration_s: float
    actual_duration_s: float
    tradeoffs: tuple          # tuple[TradeOffRecord]
    read_speed_cps: float

    @property
    def all_lines(self):
        lines = []
        for screen in self.screens:
            lines.extend(screen.lines)
        return tuple(lines)

    def to_dict(self):
        return {
            "subtitle_id": self.subtitle_id,
            "segment_ref": self.segment_ref,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "screens": [s.to_dict() for s in self.screens],
            "required_duration_s": self.required_duration_s,
            "actual_duration_s": self.actual_duration_s,
            "tradeoffs": [t.to_dict() for t in self.tradeoffs],
            "read_speed_cps": self.read_speed_cps,
        }


# Subtitle-level state machine states.
STATE_PENDING = "pending"
STATE_LAYOUT = "layout"
STATE_COMMITTED = "committed"
STATE_FAILED = "failed"

LEGAL_TRANSITIONS = {
    STATE_PENDING: {STATE_LAYOUT},
    STATE_LAYOUT: {STATE_COMMITTED, STATE_FAILED},
    STATE_FAILED: {STATE_PENDING, STATE_LAYOUT},
    STATE_COMMITTED: set(),
}
