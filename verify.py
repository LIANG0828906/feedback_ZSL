# 独立结果验证工具：仅从持久化台词结果与工程清单出发重新校验。
from __future__ import annotations

import argparse
import json
import sys

import numpy as np

from caption_pipeline.checkpoint import CheckpointStore
from caption_pipeline.engine import ALGO_VERSION
from caption_pipeline.envelope import detect_segments
from caption_pipeline.errors import E_FIT, E_SEG, PipelineError
from caption_pipeline.fit import reconcile
from caption_pipeline.layout import line_char_counts, split_screens, wrap_text
from caption_pipeline.loader import load_project
from caption_pipeline.windows import Window, derive_windows, min_readable_samples

EXIT_OK = 0
EXIT_VIOLATION = 2
EXIT_SEG = 3
EXIT_SNAPSHOT = 4


class Violation:
    def __init__(self, line_id, item, measured, code=None):
        self.line_id = line_id
        self.item = item
        self.measured = measured
        self.code = code

    def __str__(self):
        base = f"台词 {self.line_id} 违规项 {self.item} 实测值 {self.measured}"
        return f"[{self.code}] {base}" if self.code else base


def _recompute_expected(project, samples):
    # 独立重算每句台词的期望窗口与折行（纯函数链，不读持久化结果）。
    detected = detect_segments(samples, project.frame_len, project.hop)
    if len(detected) != len(project.segments):
        raise PipelineError(E_SEG, "检测段数与清单不一致",
                            details={"declared": len(project.segments),
                                     "detected": len(detected)})
    windows = derive_windows(project, detected, total_samples=len(samples))
    pres = project.presentation
    order = project.timeline_order()
    expected = {}
    for idx, line in enumerate(order):
        w = windows[line.id]
        lines = wrap_text(line.text, pres.max_chars_per_line, pres.wrap_weight)
        screens = split_screens(lines, pres.line_height, pres.margin,
                                pres.screen_height)
        if idx + 1 < len(order):
            boundary = windows[order[idx + 1].id].start
        else:
            boundary = len(samples)
        gap = max(0, boundary - w.end)
        w2, screens2, tradeoffs = reconcile(
            line, Window(w.start, w.end, w.deficit), screens, pres,
            project.sample_rate, gap)
        expected[line.id] = {
            "start": w2.start, "end": w2.end, "screens": screens2,
            "tradeoffs": [t.to_dict() for t in tradeoffs],
        }
    return expected


def verify_project(manifest_path, persist_dir, out=print,
                   tolerance_frames=1):
    project = load_project(manifest_path)
    store = CheckpointStore(persist_dir)
    try:
        meta, records = store.inspect()
    finally:
        store.close()

    expected_meta = {"content_hash": project.content_hash,
                     "param_version": project.presentation.param_version,
                     "algo_version": ALGO_VERSION}
    for key, val in expected_meta.items():
        if meta.get(key) != val:
            out(f"[INVALID_SNAPSHOT] 检查点版本不匹配: {key} "
                f"期望 {val[:16]} 实测 {str(meta.get(key))[:16]}，"
                f"快照应整体作废并全量重算")
            return EXIT_SNAPSHOT

    samples = np.load(project.audio_path)
    try:
        expected = _recompute_expected(project, samples)
    except PipelineError as exc:
        out(f"[{exc.code}] 期望结果重算失败: {exc}")
        return EXIT_SEG if exc.code == E_SEG else EXIT_VIOLATION

    committed = [r for r in records if r["state"] == "COMMITTED"]
    seg_map = project.segment_map()
    line_by_id = {l.id: l for l in project.subtitles}
    truth_by_id = {t["id"]: t
                   for t in project.ground_truth.get("lines", [])}
    tolerance = project.frame_len * tolerance_frames
    violations = []
    seg_errors = []
    prev_end = None

    for rec in committed:
        lid = rec["line_id"]
        line = line_by_id.get(lid)
        seg = seg_map.get(line.segment_id) if line else None
        # 坏段链路：引用或窗口越界 -> E_SEG，可重试，不污染其他台词。
        if line is None or seg is None or not (
                seg.start <= rec["win_start"] <= rec["win_end"]):
            seg_errors.append(Violation(lid, "segment_ref",
                                        f"[{rec['win_start']}, {rec['win_end']}]",
                                        code=E_SEG))
            continue
        exp = expected.get(lid)
        if exp is None:
            violations.append(Violation(lid, "expected_missing", lid))
            continue
        if (rec["win_start"], rec["win_end"]) != (exp["start"], exp["end"]):
            violations.append(Violation(
                lid, "window", (rec["win_start"], rec["win_end"])))
            continue
        if rec["screens"] != exp["screens"]:
            violations.append(Violation(lid, "layout", rec["screens"]))
            continue
        if prev_end is not None and rec["win_start"] < prev_end:
            violations.append(Violation(lid, "overlap", rec["win_start"]))
            continue
        prev_end = rec["win_end"]
        need = min_readable_samples(len(line.text), line.max_cps,
                                    project.sample_rate)
        if rec["win_end"] - rec["win_start"] < need:
            violations.append(Violation(
                lid, "reading_speed", rec["win_end"] - rec["win_start"]))
            continue
        truth = truth_by_id.get(lid)
        if truth:
            counts = line_char_counts(rec["screens"])
            if counts != truth["line_chars"] or \
                    sum(len(sc) for sc in rec["screens"]) != truth["num_lines"]:
                violations.append(Violation(lid, "ground_truth_layout", counts))
                continue
            if (rec["win_start"], rec["win_end"]) != (
                    truth["display_start"], truth["display_end"]):
                violations.append(Violation(
                    lid, "ground_truth_window",
                    (rec["win_start"], rec["win_end"])))
                continue

    if committed:
        duration = committed[-1]["win_end"] - committed[0]["win_start"]
        truth_total = project.ground_truth.get("total_duration")
        if truth_total is not None and len(committed) == len(truth_by_id) \
                and abs(duration - truth_total) > tolerance:
            violations.append(Violation(
                committed[-1]["line_id"], "total_duration", duration))

    if seg_errors:
        first = seg_errors[0]
        out(f"{first}  -> 走 E_SEG 链路，可修复后重试，其他台词不受影响")
        return EXIT_SEG
    if violations:
        first = violations[0]
        out(f"首个违规: {first}")
        return EXIT_VIOLATION
    out(f"验证通过: {len(committed)}/{project.total_lines} 句台词，"
        f"窗口不重叠、阅读速度达标、折行与真值一致、总时长在容差内")
    return EXIT_OK


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description="字幕结果独立验证工具")
    ap.add_argument("--project", required=True, help="工程清单 manifest.json")
    ap.add_argument("--persist", required=True, help="检查点/结果持久化目录")
    args = ap.parse_args()
    sys.exit(verify_project(args.project, args.persist))
