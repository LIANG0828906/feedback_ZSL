"""媒体工程领域数据模型(不可变)。"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional


@dataclass(frozen=True)
class SpeechSegment:
    """清单声明的语音包络段, 起止为真值采样点 [start, end)。"""

    seg_id: int
    start: int
    end: int


@dataclass(frozen=True)
class Subtitle:
    """一句角色台词。window 为可选显式显示窗口 [start, end)。"""

    sub_id: int
    text: str
    segment: int
    window: Optional[tuple[int, int]] = None


@dataclass(frozen=True)
class AssistParams:
    """朗读辅助参数: 语速 / 高亮推进方式 / 逐字时长权重(每句一组正整数)。"""

    speed: float
    highlight_mode: str               # "weighted" | "uniform"
    char_weights: tuple[tuple[int, ...], ...]   # 按句号索引


@dataclass(frozen=True)
class Truth:
    """解析真值: 每句期望窗口、每字符期望高亮起始时刻、总朗读时长。"""

    windows: tuple[tuple[int, int], ...]
    char_starts: tuple[tuple[int, ...], ...]
    total_read_samples: int


@dataclass(frozen=True)
class Project:
    """完整媒体工程(清单 + 包络数据句柄)。"""

    project_id: str
    sample_rate: int
    frame_len: int
    hop: int
    total_samples: int
    tolerance_samples: int
    segment_duration: int             # 分段推进的固定段长(采样点)
    segments: tuple[SpeechSegment, ...]
    subtitles: tuple[Subtitle, ...]
    assist: AssistParams
    truth: Truth
    envelope_path: str                # .npy 包络文件路径
    manifest_path: str = ""
    content_hash: str = ""

    @property
    def num_chunks(self) -> int:
        n, d = self.total_samples, self.segment_duration
        return (n + d - 1) // d
