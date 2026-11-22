"""本地确定性样本生成器。

按固定种子产出多条媒体工程, 不依赖网络与外部素材:
- 包络: 段内恒定幅度 0.8, 段外数字静音 0.0 (float32 .npy);
- 清单: manifest.json, 含语音段 / 字幕 / 朗读辅助参数 / 解析真值;
- 真值: 由共享流水线(pipeline)在清单声明的参考帧参数下计算。
"""

from __future__ import annotations

import json
import os

import numpy as np

from . import pipeline
from .model import AssistParams, Project, SpeechSegment, Subtitle, Truth
from .schedule import total_read_samples

_FRAME_CHOICES = ((480, 240), (512, 256), (960, 480))
_SPEEDS = (1.0, 1.25, 0.75)
_CHAR_POOL = "的了吗是在不有和人这中大为上个国我以要他时来用们生到作地于出就分对成会可主发年动同工也能下过子说产种面而方后多定行学法所民得经十三之进着等部度家电力里如水化高自二理起小物现实加量都两体制机当使点从业本去把性好应开它合还因由其些然前外天政四日那社义事平形相全表间样与关各重新线内数正心反你明看原又么利比或但质气第向道命此变条只没结解问意建月公无系军很情者最立代想已通并提直题党程展五果料象员革位入常文总次品式活设及管特件长求老头基资边流路级少图山统接知较将组见计别她手角期根论运农指几九区强放决西被干做必战先回则任取据处理世解"


def _make_text(rng: np.random.Generator, n: int) -> str:
    idx = rng.integers(0, len(_CHAR_POOL), size=n)
    return "".join(_CHAR_POOL[i] for i in idx)


def generate_project(out_dir: str, project_id: str, seed: int) -> str:
    """生成一条媒体工程, 返回工程目录。"""
    rng = np.random.default_rng(seed)
    sample_rate = 48000
    frame_len, hop = _FRAME_CHOICES[seed % len(_FRAME_CHOICES)]
    speed = _SPEEDS[seed % len(_SPEEDS)]
    segment_duration = 2 * sample_rate

    # 1. 布局语音段: 顺序放置, 段间留足静音间隙
    n_segments = int(rng.integers(3, 7))
    segments: list = []
    cursor = int(rng.integers(sample_rate // 4, sample_rate // 2))
    for _ in range(n_segments):
        seg_len = int(rng.uniform(0.8, 2.2) * sample_rate)
        segments.append((cursor, cursor + seg_len))
        cursor += seg_len + int(rng.uniform(0.5, 1.0) * sample_rate)
    total_samples = cursor + int(rng.uniform(0.2, 0.5) * sample_rate)

    # 2. 合成包络: 段内 0.8, 段外 0.0
    envelope = np.zeros(total_samples, dtype=np.float32)
    for s, e in segments:
        envelope[s:e] = 0.8

    # 3. 字幕: 每段 1~2 句, 部分句子给显式窗口(留足量化余量)
    subtitles: list = []
    margin = hop  # 显式窗口相对段边界内缩, 保证不越出检测段
    for seg_id, (s, e) in enumerate(segments):
        n_subs = int(rng.integers(1, 3))
        for _ in range(n_subs):
            text = _make_text(rng, int(rng.integers(4, 15)))
            entry: dict = {"text": text, "segment": seg_id, "window": None}
            if n_subs == 1 and rng.random() < 0.3:
                w0 = s + margin + int(rng.integers(0, (e - s) // 8))
                w1 = e - margin - int(rng.integers(0, (e - s) // 8))
                if w1 - w0 > frame_len:
                    entry["window"] = [w0, w1]
            subtitles.append(entry)

    # 4. 朗读辅助参数
    char_weights = [
        [int(w) for w in rng.integers(1, 5, size=len(sub["text"]))] for sub in subtitles
    ]
    assist = {"speed": speed, "highlight_mode": "weighted", "char_weights": char_weights}

    # 5. 用共享流水线求真值
    proj_dir = os.path.join(out_dir, project_id)
    os.makedirs(proj_dir, exist_ok=True)
    envelope_rel = f"{project_id}_envelope.npy"
    envelope_path = os.path.join(proj_dir, envelope_rel)
    np.save(envelope_path, envelope)

    stub = Project(
        project_id=project_id,
        sample_rate=sample_rate,
        frame_len=frame_len,
        hop=hop,
        total_samples=total_samples,
        tolerance_samples=frame_len,
        segment_duration=segment_duration,
        segments=tuple(SpeechSegment(i, s, e) for i, (s, e) in enumerate(segments)),
        subtitles=tuple(
            Subtitle(i, sub["text"], sub["segment"],
                     tuple(sub["window"]) if sub["window"] else None)
            for i, sub in enumerate(subtitles)
        ),
        assist=AssistParams(
            speed=speed,
            highlight_mode="weighted",
            char_weights=tuple(tuple(g) for g in char_weights),
        ),
        truth=Truth(windows=(), char_starts=(), total_read_samples=0),
        envelope_path=envelope_path,
    )
    detected = pipeline.compute_detection(stub, envelope)
    windows = pipeline.compute_windows(stub, detected)
    char_starts = [
        pipeline.compute_sentence_schedule(stub, i, w) for i, w in enumerate(windows)
    ]
    total_window = sum(w1 - w0 for w0, w1 in windows)
    truth = {
        "windows": [[int(a), int(b)] for a, b in windows],
        "char_starts": [[int(t) for t in starts] for starts in char_starts],
        "total_read_samples": total_read_samples(total_window, speed),
    }

    manifest = {
        "project_id": project_id,
        "seed": seed,
        "time_unit": "samples",
        "sample_rate": sample_rate,
        "frame_len": frame_len,
        "hop": hop,
        "total_samples": total_samples,
        "tolerance_samples": frame_len,
        "segment_duration": segment_duration,
        "envelope_file": envelope_rel,
        "segments": [{"start": s, "end": e} for s, e in segments],
        "subtitles": subtitles,
        "assist": assist,
        "truth": truth,
    }
    with open(os.path.join(proj_dir, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=1)
    return proj_dir


def generate_all(out_dir: str, count: int = 4, base_seed: int = 42) -> list:
    os.makedirs(out_dir, exist_ok=True)
    return [
        generate_project(out_dir, f"proj_{i:03d}", seed=base_seed + i)
        for i in range(count)
    ]
