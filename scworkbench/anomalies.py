from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class FragmentFault:
    frame_seq: int
    fragments: tuple[int, ...]
    attempts: int | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_seq": self.frame_seq,
            "fragments": list(self.fragments),
            "attempts": self.attempts,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "FragmentFault":
        return cls(
            frame_seq=int(data["frame_seq"]),
            fragments=tuple(int(v) for v in data["fragments"]),
            attempts=(
                None if data.get("attempts") is None else int(data["attempts"])
            ),
        )


@dataclass
class AnomalyConfig:
    name: str = "clean"
    description: str = "无异常"
    loss: list[FragmentFault] = field(default_factory=list)
    corrupt: list[FragmentFault] = field(default_factory=list)
    reorder_frames: list[int] = field(default_factory=list)
    capability_change_frame: int | None = None
    capability_change_side: str = "receiver"
    capability_change_codecs: list[str] | None = None
    capability_change_max_width: int | None = None
    capability_change_max_height: int | None = None
    duplicate_inputs: list[int] = field(default_factory=list)
    session_mismatch_frames: list[int] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "AnomalyConfig":
        if not data:
            return cls()
        return cls(
            name=str(data.get("name", "custom")),
            description=str(data.get("description", "自定义异常")),
            loss=[
                FragmentFault.from_dict(item) for item in data.get("loss", [])
            ],
            corrupt=[
                FragmentFault.from_dict(item) for item in data.get("corrupt", [])
            ],
            reorder_frames=[
                int(item) for item in data.get("reorder_frames", [])
            ],
            capability_change_frame=(
                None
                if data.get("capability_change_frame") is None
                else int(data["capability_change_frame"])
            ),
            capability_change_side=str(
                data.get("capability_change_side", "receiver")
            ),
            capability_change_codecs=(
                None
                if data.get("capability_change_codecs") is None
                else [str(item) for item in data["capability_change_codecs"]]
            ),
            capability_change_max_width=(
                None
                if data.get("capability_change_max_width") is None
                else int(data["capability_change_max_width"])
            ),
            capability_change_max_height=(
                None
                if data.get("capability_change_max_height") is None
                else int(data["capability_change_max_height"])
            ),
            duplicate_inputs=[
                int(item) for item in data.get("duplicate_inputs", [])
            ],
            session_mismatch_frames=[
                int(item) for item in data.get("session_mismatch_frames", [])
            ],
        )

    def loss_faults_for(self, frame_seq: int) -> list[FragmentFault]:
        return [fault for fault in self.loss if fault.frame_seq == frame_seq]

    def corrupt_faults_for(self, frame_seq: int) -> list[FragmentFault]:
        return [fault for fault in self.corrupt if fault.frame_seq == frame_seq]

    def should_reorder(self, frame_seq: int) -> bool:
        return frame_seq in self.reorder_frames


def _fault(
    frame_seq: int,
    fragments: list[int],
    attempts: int | None = None,
) -> dict[str, Any]:
    return {
        "frame_seq": frame_seq,
        "fragments": fragments,
        "attempts": attempts,
    }


def preset_payloads() -> dict[str, dict[str, Any]]:
    return {
        "clean": {
            "name": "clean",
            "description": "无异常，用于基线演示",
        },
        "loss": {
            "name": "loss",
            "description": "第 7 帧首传丢片并由重传恢复；第 18 帧持续丢片",
            "loss": [_fault(7, [2], 1), _fault(18, [1], None)],
        },
        "reorder": {
            "name": "reorder",
            "description": "第 9、23 帧分片逆序到达，接收端仍按序号重组",
            "reorder_frames": [9, 23],
        },
        "capability": {
            "name": "capability",
            "description": "第 16 帧收到降级能力，拒绝并保留协商基线",
            "capability_change_frame": 16,
            "capability_change_side": "receiver",
            "capability_change_codecs": ["h264-baseline"],
            "capability_change_max_width": 480,
            "capability_change_max_height": 270,
        },
        "corrupt": {
            "name": "corrupt",
            "description": "第 12 帧分片校验失败，触发重传并产生损坏诊断",
            "corrupt": [_fault(12, [0], 1)],
        },
        "mixed": {
            "name": "mixed",
            "description": "组合丢包、乱序、能力变化、损坏与输入重复",
            "loss": [_fault(6, [1], 1), _fault(20, [0], None)],
            "reorder_frames": [9, 24],
            "corrupt": [_fault(12, [0], 1)],
            "capability_change_frame": 16,
            "capability_change_side": "receiver",
            "capability_change_codecs": ["h264-baseline"],
            "capability_change_max_width": 480,
            "capability_change_max_height": 270,
            "duplicate_inputs": [7],
            "session_mismatch_frames": [28],
        },
    }


def default_presets() -> dict[str, AnomalyConfig]:
    return {
        name: AnomalyConfig.from_dict(payload)
        for name, payload in preset_payloads().items()
    }


def write_presets(path: str | Path) -> None:
    Path(path).write_text(
        json.dumps(
            preset_payloads(),
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )


def load_presets(path: str | Path) -> dict[str, AnomalyConfig]:
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    return {
        name: AnomalyConfig.from_dict(payload)
        for name, payload in data.items()
    }
