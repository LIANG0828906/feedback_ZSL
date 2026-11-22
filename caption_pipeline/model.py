"""工程数据模型。"""
from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Segment:
    id: int
    start: int
    end: int


@dataclass(frozen=True)
class Presentation:
    max_chars_per_line: int
    line_height: int
    margin: int
    screen_height: int
    wrap_weight: float
    param_version: str


@dataclass(frozen=True)
class Subtitle:
    id: str
    text: str
    segment_id: int
    max_cps: float
    window: tuple | None = None  # 可选显式显示窗口 [start, end)


@dataclass
class Project:
    project_id: str
    sample_rate: int
    frame_len: int
    hop: int
    audio_path: str
    segments: list
    presentation: Presentation
    subtitles: list
    ground_truth: dict
    manifest_dir: str
    content_hash: str

    @property
    def total_lines(self) -> int:
        return len(self.subtitles)

    def segment_map(self) -> dict:
        return {s.id: s for s in self.segments}

    def timeline_order(self) -> list:
        """按时间轴顺序（段起点、台词编号）返回台词列表，顺序稳定。"""
        segmap = self.segment_map()
        return sorted(self.subtitles, key=lambda l: (segmap[l.segment_id].start, l.id))

    def summary(self) -> dict:
        total_samples = max((s.end for s in self.segments), default=0)
        return {
            "project_id": self.project_id,
            "sample_rate": self.sample_rate,
            "frame_len": self.frame_len,
            "hop": self.hop,
            "segments": len(self.segments),
            "total_lines": self.total_lines,
            "total_chars": sum(len(l.text) for l in self.subtitles),
            "timeline_samples": total_samples,
            "param_version": self.presentation.param_version,
            "content_hash": self.content_hash[:16],
        }
