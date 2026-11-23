"""本地确定性样本生成器（固定随机种子，无需网络）。

为每个工程产出：

* 三个版本格式的工程描述（v1/v2/v3，字段命名/分帧表达/阶段清单/音频引用各异）
* 干净参考 PCM 与含噪 PCM（int16 LE，单声道或立体声）
* 解析真值清单：静音段起止采样点、SNR 提升下限、噪声基底残留标准差上界
* 两个损坏工程样本（不可解码 JSON / 必填字段缺失）用于异常链路测试

真值中的统计阈值由生成器对 *当前算法版本* 做一次参考运行后带保护边际写入，
观测依据仍是修复前后采样的逐样本比对（见 tests 与 scripts/run_demo.py）。
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from .core.engine import RestorationEngine, build_windows
from .core.metrics import segment_stats
from .core.pcm import write_pcm
from .model import RestoreProject
from .formats.v1 import export as export_v1
from .formats.v2 import export as export_v2
from .formats.v3 import export as export_v3

SEED = 20260101
SAMPLE_RATE = 24000
FRAME_SIZE = 512
HOP_SIZE = 128
WINDOW_SECONDS = 1.0


def _tone(rng: np.random.Generator, duration: float, channels: int) -> np.ndarray:
    count = int(SAMPLE_RATE * duration)
    t = np.arange(count, dtype=np.float64) / SAMPLE_RATE
    # 语音/音乐素材的简化模型：两个正弦分量 + 慢包络
    envelope = 0.6 * (0.5 + 0.5 * np.sin(2 * np.pi * 3.0 * t))
    mono = envelope * (
        0.7 * np.sin(2 * np.pi * 220.0 * t) + 0.3 * np.sin(2 * np.pi * 440.0 * t)
    )
    # 强制静音段（真值记录的区间）：每 0.5 秒静音 0.25 秒
    silence_ranges: list[tuple[int, int]] = []
    cursor = 0
    while cursor < count:
        start = min(cursor + int(0.25 * SAMPLE_RATE), count)
        end = min(start + int(0.25 * SAMPLE_RATE), count)
        if end > start:
            mono[start:end] = 0.0
            silence_ranges.append((start, end))
        cursor = end
    noise = rng.normal(0.0, 0.08, size=count)
    noisy_mono = np.clip(mono + noise, -1.0, 1.0)
    if channels == 1:
        clean = mono[:, None]
        noisy = noisy_mono[:, None]
    else:
        # 立体声：右通道相对左通道有半帧延迟，噪声独立
        shifted = np.roll(mono, FRAME_SIZE // 2)
        shifted[: FRAME_SIZE // 2] = 0.0
        clean = np.stack([mono, shifted], axis=1)
        noise_r = rng.normal(0.0, 0.08, size=count)
        noisy = np.clip(
            np.stack([noisy_mono, shifted + noise_r], axis=1), -1.0, 1.0
        )
    return clean.astype(np.float64), noisy.astype(np.float64), silence_ranges


def _base_project(project_id: str, channels: int, pcm_name: str) -> RestoreProject:
    from .model import AudioRef, Framing, StageConfig, StageKind

    stages = (
        StageConfig(
            StageKind.SPECTRAL_SUBTRACTION,
            True,
            {"strength": 1.4, "noise_frames": 8},
        ),
        StageConfig(
            StageKind.MASK_SMOOTHING,
            True,
            {"smoothing": 0.5, "floor_db": -12.0},
        ),
        StageConfig(
            StageKind.NOISE_GATE,
            True,
            {"threshold_db": -45.0, "attack_ms": 5.0, "release_ms": 50.0},
        ),
    )
    return RestoreProject(
        project_id=project_id,
        source_version="3.0",
        framing=Framing(FRAME_SIZE, HOP_SIZE, WINDOW_SECONDS, 512),
        stages=stages,
        audio=AudioRef(
            path=pcm_name,
            sample_rate=SAMPLE_RATE,
            channels=channels,
            sample_width_bytes=2,
        ),
        window_seconds=WINDOW_SECONDS,
        seed=SEED,
    )


def _write_version_docs(project: RestoreProject, out_dir: Path) -> dict[str, Path]:
    docs = {
        "1.0": export_v1(project)[0],
        "2.0": export_v2(project)[0],
        "3.0": export_v3(project)[0],
    }
    paths: dict[str, Path] = {}
    for version, doc in docs.items():
        target = out_dir / f"{project.project_id}.v{version}.json"
        with open(target, "w", encoding="utf-8") as handle:
            json.dump(doc, handle, ensure_ascii=False, indent=2)
        paths[version] = target
    return paths


def _reference_truth(
    project: RestoreProject,
    project_dir: Path,
    clean: np.ndarray,
    noisy: np.ndarray,
    silence_ranges: list[tuple[int, int]],
) -> dict:
    """用当前算法做参考运行，逐窗口统计并生成带保护边际的真值阈值。"""
    audio_path = project_dir / project.audio.path
    ckpt_dir = project_dir / "_reference_ckpt"
    engine = RestorationEngine(project, noisy, ckpt_dir, audio_path, max_workers=1)
    restored = engine.run_incremental()
    windows = build_windows(noisy.shape[0], SAMPLE_RATE, WINDOW_SECONDS)
    segments = []
    for slice_ in windows:
        local_silence = [
            (max(start - slice_.start, 0), end - slice_.start)
            for start, end in silence_ranges
            if end > slice_.start and start < slice_.end
        ]
        stats = segment_stats(
            noisy[slice_.start:slice_.end],
            restored[slice_.start:slice_.end],
            clean[slice_.start:slice_.end],
            local_silence,
            slice_.index,
        )
        segments.append(
            {
                "window_index": slice_.index,
                "start_sample": slice_.start,
                "end_sample": slice_.end,
                # 保护边际：SNR 下限留出 1 dB，残留上界放宽 20%
                "snr_gain_db_min": round(stats.snr_gain_db - 1.0, 4),
                "residual_std_max": round(stats.residual_std * 1.2 + 1e-6, 6),
                "observed_snr_gain_db": round(stats.snr_gain_db, 4),
                "observed_residual_std": round(stats.residual_std, 6),
            }
        )
    # 参考检查点不作为交付样本，删除以免与正式运行混淆
    import shutil

    shutil.rmtree(ckpt_dir, ignore_errors=True)
    return {
        "sample_rate": SAMPLE_RATE,
        "channels": project.audio.channels,
        "silence_ranges": [[start, end] for start, end in silence_ranges],
        "segments": segments,
    }


def generate(root: str | Path) -> Path:
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(SEED)
    specs = [
        ("voice_mono", 1, 3.0),
        ("music_stereo", 2, 3.5),
        ("sfx_mono", 1, 2.5),
        ("ambience_stereo", 2, 4.0),
    ]
    manifest: list[dict] = []
    for project_id, channels, duration in specs:
        project_dir = root / project_id
        project_dir.mkdir(parents=True, exist_ok=True)
        clean, noisy, silence_ranges = _tone(rng, duration, channels)
        pcm_name = f"{project_id}.noisy.pcm"
        clean_name = f"{project_id}.clean.pcm"
        write_pcm(project_dir / pcm_name, noisy)
        write_pcm(project_dir / clean_name, clean)

        project = _base_project(project_id, channels, pcm_name)
        doc_paths = _write_version_docs(project, project_dir)
        truth = _reference_truth(project, project_dir, clean, noisy, silence_ranges)
        truth_path = project_dir / f"{project_id}.truth.json"
        with open(truth_path, "w", encoding="utf-8") as handle:
            json.dump(truth, handle, ensure_ascii=False, indent=2)
        manifest.append(
            {
                "project_id": project_id,
                "channels": channels,
                "duration_seconds": duration,
                "documents": {version: str(path.name) for version, path in doc_paths.items()},
                "noisy_pcm": pcm_name,
                "clean_pcm": clean_name,
                "truth": truth_path.name,
            }
        )

    _write_corrupt_samples(root)
    with open(root / "manifest.json", "w", encoding="utf-8") as handle:
        json.dump({"seed": SEED, "projects": manifest}, handle, ensure_ascii=False, indent=2)
    return root


def _write_corrupt_samples(root: Path) -> None:
    corrupt_dir = root / "corrupt"
    corrupt_dir.mkdir(exist_ok=True)
    # 1) 可解码但必填字段缺失
    broken_field = {
        "format_version": "2.0",
        "projectId": "broken_missing_field",
        "framing": {"frameSamples": 512, "hopSamples": 128, "windowSec": 1.0},
        # 故意缺少 audioRef
        "stageList": [],
    }
    with open(corrupt_dir / "missing_field.v2.json", "w", encoding="utf-8") as handle:
        json.dump(broken_field, handle, ensure_ascii=False, indent=2)
    # 2) 文件存在但 JSON 不可解码
    with open(corrupt_dir / "undecodable.json", "wb") as handle:
        handle.write(b'{"format_version": "1.0", "frame": {,}, broken json bytes \xff')
    # 3) 未知版本号
    unknown_version = {"format_version": "9.9", "project_id": "future"}
    with open(corrupt_dir / "unknown_version.json", "w", encoding="utf-8") as handle:
        json.dump(unknown_version, handle, ensure_ascii=False, indent=2)


if __name__ == "__main__":
    import sys

    target = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("samples")
    generate(target)
    print(f"samples generated at {target} (seed={SEED})")
