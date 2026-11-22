"""Deterministic local sample generator.

Running :func:`generate_all` with a fixed seed reproduces byte-identical
projects: synthetic voiced audio envelopes, subtitle descriptions, rendering
parameters and an analytic ground-truth manifest.
"""

import json
import os

import numpy as np

from .hashing import canonical_json, content_hash
from .model import PresentationParams

GENERATOR_VERSION = "1.0.0"

# Punctuation that must never start a visual line (no orphan punctuation).
NO_LINE_START_PUNCT = "，。、；：？！）】》」』”’…—.,;:!?)]}\"'"

# Fixed text pool so output never depends on locale word lists.
TEXT_POOL = [
    "警报响起，舱门即将关闭。",
    "我们必须在能源耗尽之前抵达控制中心。",
    "前方通道有敌方巡逻单位，保持静默。",
    "这把钥匙能打开地下机库的旧闸门。",
    "别担心，我会掩护你穿过中庭。",
    "信号中断了，最后坐标停留在废弃观测站附近。",
    "如果计划失败，就沿通风管道撤离到地表。",
    "记住，真正的敌人一直藏在我们身后。",
]


def _rng(seed, tag):
    tag_digest = int.from_bytes(
        __import__("hashlib").sha256(str(tag).encode("utf-8")).digest()[:4], "big")
    return np.random.default_rng(np.random.SeedSequence([seed, tag_digest]))


def _synthesize_waveform(rng, sample_rate, segments, frame_shift,
                         frame_length=400):
    total = max(seg.end_sample for seg in segments) + sample_rate
    wave = rng.normal(0.0, 0.002, total).astype(np.float64)
    for idx, seg in enumerate(segments):
        start = (seg.start_sample // frame_shift) * frame_shift
        end = ((seg.end_sample + frame_shift - 1) // frame_shift) * frame_shift
        t = np.arange(end - start) / sample_rate
        freq = 160.0 + 35.0 * (idx % 5)
        tone = 0.6 * np.sin(2 * np.pi * freq * t)
        # Smooth attack/release envelope so voiced energy is unambiguous.
        ramp = min(int(0.01 * sample_rate), (end - start) // 4)
        env = np.ones_like(t)
        env[:ramp] = np.linspace(0, 1, ramp)
        env[-ramp:] = np.linspace(1, 0, ramp)
        wave[start:end] += tone * env
    # Silence markers are applied last (otherwise voiced tones overwrite them).
    ordered = sorted(segments, key=lambda s: s.start_sample)
    for prev, nxt in zip(ordered, ordered[1:]):
        gap = nxt.start_sample - prev.end_sample
        if 0 < gap <= frame_length:
            # Zero only the real gap; a frame-grid-aligned gap of one frame
            # shift yields a near-silence frame the detector can split on.
            wave[prev.end_sample:nxt.start_sample] = 0.0
        elif gap == 0:
            wave[prev.end_sample - frame_length // 2:
                 prev.end_sample + frame_length // 2] = 0.0
    return wave


def _params_variant(rng, kind):
    if kind == "narrow":
        return PresentationParams(
            line_height_px=32, max_chars_per_line=6, vertical_margin_px=24,
            screen_height_px=200, wrap_weight_raggedness=1.0,
            wrap_weight_break=0.5, min_line_dwell_s=0.2,
            line_dwell_s=0.05, screen_gap_s=0.08,
        )
    if kind == "tiny":
        # Width smaller than any atom -> E_PARA for CJK subtitles.
        return PresentationParams(
            line_height_px=30, max_chars_per_line=0, vertical_margin_px=10,
            screen_height_px=120,
        )
    base = PresentationParams(
        line_height_px=30, max_chars_per_line=10, vertical_margin_px=30,
        screen_height_px=240, wrap_weight_raggedness=1.0,
        wrap_weight_break=0.5,
    )
    return base


def _build_segments(rng, sample_rate, frame_shift, count, tight=False,
                    frame_length=400):
    segments = []
    cursor = sample_rate  # 1 s leading silence
    for i in range(count):
        if tight == "long":
            duration_s = float(rng.uniform(3.0, 4.0))
        else:
            duration_s = float(rng.uniform(1.8, 2.8))
        start = ((cursor + frame_shift - 1) // frame_shift) * frame_shift
        end = start + int(round(duration_s * sample_rate))
        end = (end // frame_shift) * frame_shift
        segments.append({"segment_id": f"seg{i+1:02d}",
                         "start_sample": int(start), "end_sample": int(end)})
        if tight == "zero":
            # Zero declared gap: no extension room at all, so readability
            # deterministically raises E_GAP. The synthesiser still writes a
            # detectable join marker without changing declared boundaries.
            gap_s = 0.0
        elif tight:
            gap_s = float(rng.uniform(0.05, 0.12))
        else:
            gap_s = float(rng.uniform(0.8, 1.4))
        cursor = end + int(round(gap_s * sample_rate))
    return segments


def _build_subtitles(rng, segments, kind):
    subtitles = []
    for i, seg in enumerate(segments):
        if kind == "tiny":
            text = f"WARNING{100 + i} " + TEXT_POOL[i % len(TEXT_POOL)]
        elif kind == "narrow":
            # Shorter lines keep the narrow-width project fully satisfiable.
            text = TEXT_POOL[i % 4]
        else:
            text = TEXT_POOL[i % len(TEXT_POOL)]
        sub = {
            "subtitle_id": i + 1,
            "text": text,
            "segment_ref": seg["segment_id"],
            "explicit_start": None,
            "explicit_end": None,
            "max_read_speed": round(float(rng.uniform(
                *(6.0, 8.0) if kind == "narrow" else (9.0, 14.0))), 2),
        }
        if kind == "main" and i % 3 == 1:
            # Explicit window fully inside the segment; still must satisfy
            # readability via the same extension rules.
            span = seg["end_sample"] - seg["start_sample"]
            sub["explicit_start"] = seg["start_sample"] + span // 8
            sub["explicit_end"] = seg["start_sample"] + span * 3 // 4
        subtitles.append(sub)
    return subtitles


def build_project_spec(seed, kind, count):
    """Return a plain-dict project specification (no ground truth yet)."""
    rng = np.random.default_rng(np.random.SeedSequence([seed, ord(kind[0]), count]))
    sample_rate = 16000
    frame_length = 400
    frame_shift = 160
    tight = "zero" if kind == "tight" else ("long" if kind == "narrow" else False)
    segments = _build_segments(rng, sample_rate, frame_shift, count,
                               tight=tight, frame_length=frame_length)
    subtitles = _build_subtitles(rng, segments, kind)
    params = _params_variant(rng, kind)
    return {
        "project_id": f"proj_{kind}_{seed}",
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "sample_rate": sample_rate,
        "frame_length": frame_length,
        "frame_shift": frame_shift,
        # Well above ramp energy (~0.06) and silence (~0.002), well below the
        # steady voiced core (~0.4), so confident runs start deterministically.
        "energy_threshold": 0.1,
        "segments": segments,
        "subtitles": subtitles,
        "params": params.to_dict(),
    }


def write_project(root, spec):
    """Materialize one project: waveform, manifest and fingerprint file."""
    project_dir = os.path.join(root, spec["project_id"])
    os.makedirs(project_dir, exist_ok=True)
    rng = np.random.default_rng(np.random.SeedSequence([spec["seed"], 77]))
    segments = [type("S", (), s) for s in spec["segments"]]
    wave = _synthesize_waveform(rng, spec["sample_rate"], segments,
                                spec["frame_shift"], spec["frame_length"])
    np.save(os.path.join(project_dir, "waveform.npy"), wave)
    manifest = {k: v for k, v in spec.items()}
    with open(os.path.join(project_dir, "manifest.json"), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, ensure_ascii=False, indent=2, sort_keys=True)
    fingerprint = content_hash(manifest)
    with open(os.path.join(project_dir, "fingerprint.txt"), "w", encoding="utf-8") as fh:
        fh.write(fingerprint + "\n")
    return project_dir, fingerprint


def write_truth(project_dir, truth):
    with open(os.path.join(project_dir, "ground_truth.json"), "w",
              encoding="utf-8") as fh:
        json.dump(truth, fh, ensure_ascii=False, indent=2, sort_keys=True)


def generate_all(root, seed=20261123):
    """Generate the fixed deterministic project bundle under ``root``."""
    os.makedirs(root, exist_ok=True)
    specs = [
        build_project_spec(seed, "main", 8),
        build_project_spec(seed + 1, "narrow", 6),
        build_project_spec(seed + 2, "tight", 6),
        build_project_spec(seed + 3, "tiny", 3),
    ]
    index = {"generator_version": GENERATOR_VERSION, "seed": seed, "projects": []}
    for spec in specs:
        project_dir, fingerprint = write_project(root, spec)
        index["projects"].append({
            "project_id": spec["project_id"],
            "dir": os.path.basename(project_dir),
            "fingerprint": fingerprint,
        })
    with open(os.path.join(root, "index.json"), "w", encoding="utf-8") as fh:
        json.dump(index, fh, ensure_ascii=False, indent=2, sort_keys=True)
    return index


def load_index(root):
    with open(os.path.join(root, "index.json"), encoding="utf-8") as fh:
        return json.load(fh)
