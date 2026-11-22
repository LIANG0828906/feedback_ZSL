# 本地确定性样本生成器：固定种子产出多条媒体工程（音频时间轴、
# 字幕描述、呈现参数与解析真值清单）。
from __future__ import annotations

import argparse
import json
import os
import random

import numpy as np

from caption_pipeline.envelope import detect_segments
from caption_pipeline.fit import reconcile
from caption_pipeline.layout import line_char_counts, split_screens, wrap_text
from caption_pipeline.windows import Window, derive_windows

SAMPLE_RATE = 16000
FRAME_LEN = 400
HOP = 160

_SENTENCES = [
    "风从山谷里吹过来带着青草的气味",
    "我们必须赶在天黑之前穿过这片森林",
    "古老的钟楼在夕阳下投下长长的影子",
    "她轻声说道，「答案就藏在那本旧日记里。」",
    "舰长下令全员进入一级战备状态！",
    "雨滴落在窗台上，像一首没有歌词的歌。",
    "他回头望了一眼，然后毫不犹豫地推开了门。",
    "这条路的尽头，就是传说中沉睡的遗迹。",
    "别怕，我会一直陪在你身边。",
    "引擎的轰鸣声渐渐远去，营地恢复了宁静。",
    "如果地图没错的话，宝藏应该就埋在这棵树下。",
    "时间不等人，我们得分头行动，在广场汇合！",
]


def _make_text(rng, min_len=8, max_len=46):
    text = rng.choice(_SENTENCES)
    while len(text) < min_len:
        text += "，" + rng.choice(_SENTENCES)
    if len(text) > max_len:
        text = text[:max_len - 1] + "。"
    return text


def _synth_audio(rng_np, segments, total):
    audio = rng_np.normal(0.0, 0.012, total)
    for start, end in segments:
        n = end - start
        burst = rng_np.normal(0.0, 0.5, n)
        attack = max(1, n // 8)
        env = np.ones(n)
        env[:attack] = np.linspace(0.2, 1.0, attack)
        env[-attack:] = np.linspace(1.0, 0.2, attack)
        audio[start:end] += burst * env
    return audio.astype(np.float64)


def _ground_truth(manifest, audio):
    # 用管线自身的确定性阶段计算解析真值（算法版本与引擎一致）。
    from caption_pipeline.model import (Presentation, Project, Segment,
                                        Subtitle)

    detected = detect_segments(audio, manifest["frame_len"], manifest["hop"])
    assert len(detected) == len(manifest["segments"]), "样本段检测不一致"

    pres_raw = manifest["presentation"]
    project = Project(
        project_id=manifest["project_id"],
        sample_rate=manifest["sample_rate"],
        frame_len=manifest["frame_len"],
        hop=manifest["hop"],
        audio_path="",
        segments=[Segment(**s) for s in manifest["segments"]],
        presentation=Presentation(**pres_raw),
        subtitles=[Subtitle(id=s["id"], text=s["text"],
                            segment_id=s["segment_id"], max_cps=s["max_cps"],
                            window=tuple(s["window"]) if s["window"] else None)
                   for s in manifest["subtitles"]],
        ground_truth={}, manifest_dir="", content_hash="",
    )
    windows = derive_windows(project, detected, total_samples=len(audio))

    pres = project.presentation
    order = project.timeline_order()
    lines_truth = []
    for idx, line in enumerate(order):
        w = windows[line.id]
        lines = wrap_text(line.text, pres.max_chars_per_line,
                          pres.wrap_weight)
        screens = split_screens(lines, pres.line_height, pres.margin,
                                pres.screen_height)
        if idx + 1 < len(order):
            boundary = windows[order[idx + 1].id].start
        else:
            boundary = len(audio)
        gap = max(0, boundary - w.end)
        w2, screens2, _ = reconcile(line, Window(w.start, w.end, w.deficit),
                                    screens, pres,
                                    project.sample_rate, gap)
        lines_truth.append({
            "id": line.id,
            "display_start": w2.start,
            "display_end": w2.end,
            "num_lines": sum(len(sc) for sc in screens2),
            "line_chars": line_char_counts(screens2),
        })
    total_duration = lines_truth[-1]["display_end"] - lines_truth[0]["display_start"]
    return {"lines": lines_truth, "total_duration": total_duration}


def generate(out_dir, seed=20261123, n_projects=3):
    os.makedirs(out_dir, exist_ok=True)
    manifests = []
    for p in range(n_projects):
        rng = random.Random(seed + p * 977)
        rng_np = np.random.default_rng(seed + p * 977)
        nseg = rng.randint(5, 8)
        segments = []
        t = rng.randint(2000, 4000)
        for i in range(nseg):
            dur = rng.randint(int(0.9 * SAMPLE_RATE), int(2.4 * SAMPLE_RATE))
            segments.append({"id": i, "start": t, "end": t + dur})
            t += dur + rng.randint(int(0.25 * SAMPLE_RATE),
                                   int(0.9 * SAMPLE_RATE))
        total = t
        audio = _synth_audio(rng_np, [(s["start"], s["end"]) for s in segments],
                             total)

        pres = {
            "max_chars_per_line": rng.randint(12, 18),
            "line_height": 30,
            "margin": rng.randint(16, 28),
            "screen_height": rng.choice([150, 180, 210]),
            "wrap_weight": 1.0,
            "param_version": "pv1",
        }
        subtitles = []
        for i, seg in enumerate(segments):
            long_line = (p == 1 and i == len(segments) // 2)
            text = _make_text(rng, min_len=40 if long_line else 8,
                              max_len=64 if long_line else 46)
            subtitles.append({
                "id": f"L{i + 1:03d}",
                "text": text,
                "segment_id": seg["id"],
                "max_cps": round(rng.uniform(6.0, 14.0), 2),
                "window": None,
            })

        manifest = {
            "project_id": f"proj_{p}",
            "sample_rate": SAMPLE_RATE,
            "frame_len": FRAME_LEN,
            "hop": HOP,
            "audio_file": f"audio_{p}.npy",
            "segments": segments,
            "presentation": pres,
            "subtitles": subtitles,
        }
        # 确定性自检：若个别台词排版冲突无法收敛，按固定步长上调
        # 阅读速度上限（仍为正数），必要时定长截短文本，直至可收敛。
        from caption_pipeline.errors import E_FIT, PipelineError
        for _attempt in range(40):
            try:
                truth = _ground_truth(manifest, audio)
                break
            except PipelineError as exc:
                if exc.code != E_FIT:
                    raise
                for bad in exc.details["lines"]:
                    sub = next(s for s in subtitles
                               if s["id"] == bad["line_id"])
                    if sub["max_cps"] < 16.0:
                        sub["max_cps"] = round(sub["max_cps"] + 1.0, 2)
                    else:
                        sub["text"] = sub["text"][:-4]
        else:
            raise RuntimeError("样本自检无法收敛")

        proj_dir = os.path.join(out_dir, f"proj_{p}")
        os.makedirs(proj_dir, exist_ok=True)
        np.save(os.path.join(proj_dir, f"audio_{p}.npy"), audio)
        manifest["ground_truth"] = truth
        path = os.path.join(proj_dir, "manifest.json")
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(manifest, fh, ensure_ascii=False, indent=2)
        manifests.append(path)
        print(f"[gen] {path}  segments={nseg} lines={len(subtitles)}")
    return manifests


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description="确定性样本生成器")
    ap.add_argument("--out", default="samples")
    ap.add_argument("--seed", type=int, default=20261123)
    ap.add_argument("--projects", type=int, default=3)
    args = ap.parse_args()
    generate(args.out, args.seed, args.projects)
