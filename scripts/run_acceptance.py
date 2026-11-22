"""Acceptance run: adaptive buffering, three-path consistency,
checkpoint resume and the error-code checks on the local jitter sample.

Usage:  .venv/bin/python scripts/run_acceptance.py
Writes: exports/acceptance_report.json, exports/acceptance_report.md,
        exports/output_realtime.npy, exports/decisions.json
"""
import json
import os
import shutil
import sys
import tempfile

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from jitter_workbench.sample_gen import generate_sample  # noqa: E402
from jitter_workbench.loader import load_sample  # noqa: E402
from jitter_workbench.controller import ControllerParams  # noqa: E402
from jitter_workbench.paths import (run_offline, run_realtime,  # noqa: E402
                                    run_resume)
from jitter_workbench.compare import compare_three  # noqa: E402
from jitter_workbench.errors import WorkbenchError  # noqa: E402

SAMPLE_DIR = os.path.join(ROOT, "samples", "voice_seed42")
CKPT_DIR = os.path.join(ROOT, "checkpoints")
EXPORT_DIR = os.path.join(ROOT, "exports")


def clean_state():
    for d in (CKPT_DIR, EXPORT_DIR):
        if os.path.isdir(d):
            shutil.rmtree(d)
        os.makedirs(d)


def check_error_codes(report):
    """Drive every error code with a mutated copy of the sample."""
    tmpdirs = []

    def mutate(fn):
        d = tempfile.mkdtemp(dir=EXPORT_DIR)
        tmpdirs.append(d)
        sd = os.path.join(d, "s")
        shutil.copytree(SAMPLE_DIR, sd)
        mp = os.path.join(sd, "manifest.json")
        with open(mp) as fh:
            m = json.load(fh)
        fn(m, sd)
        with open(mp, "w") as fh:
            json.dump(m, fh)
        return sd

    cases = {}

    def expect(code, make):
        try:
            load_sample(make())
            cases[code] = "FAIL (no error raised)"
        except WorkbenchError as exc:
            cases[code] = ("OK" if exc.code == code
                           else "FAIL (got %s)" % exc.code)

    expect("E_META", lambda: mutate(lambda m, _: m.pop("sample_rate")))
    expect("E_JITTER",
           lambda: mutate(lambda m, _: m["jitter_patterns"].append("x")))

    def dec(_m, sd):
        fp = os.path.join(sd, "frames.bin")
        with open(fp, "r+b") as fh:
            fh.truncate(100)
    expect("E_DECODE", lambda: mutate(dec))

    def order(m, _):
        for e in m["frames"]:
            e["tags"] = [t for t in e["tags"] if t != "reorder"]
        a, b = m["arrivals"][10], m["arrivals"][11]
        a["arrival_ts"], b["arrival_ts"] = b["arrival_ts"], a["arrival_ts"]
        m["arrivals"][10], m["arrivals"][11] = b, a
    expect("E_ORDER", lambda: mutate(order))

    def dup(m, _):
        base = dict(m["arrivals"][0])
        dups = []
        for i in range(30):
            d2 = dict(base)
            d2["arrival_ts"] = base["arrival_ts"] + 1 + i
            d2["dup"] = True
            dups.append(d2)
        m["arrivals"][1:1] = dups
    expect("E_DUP", lambda: mutate(dup))

    try:
        ControllerParams(init_delay_ms=99999).validate()
        cases["E_PARA"] = "FAIL (no error raised)"
    except WorkbenchError as exc:
        cases["E_PARA"] = "OK" if exc.code == "E_PARA" else "FAIL"

    for d in tmpdirs:
        shutil.rmtree(d, ignore_errors=True)
    report["error_codes"] = cases


def main():
    report = {"sample_dir": SAMPLE_DIR, "ckpt_dir": CKPT_DIR,
              "export_dir": EXPORT_DIR}
    clean_state()
    report["clean_state"] = "checkpoints/exports cleared"

    manifest = generate_sample(SAMPLE_DIR)
    report["sample"] = {"id": manifest["sample_id"],
                        "seed": manifest["seed"],
                        "frames": manifest["num_frames"],
                        "arrivals": len(manifest["arrivals"]),
                        "jitter_patterns": manifest["jitter_patterns"]}

    sample = load_sample(SAMPLE_DIR)
    params = ControllerParams()
    report["content_hash"] = sample.content_hash
    report["policy_version"] = params.version

    # ---- three paths ---------------------------------------------------
    r_off = run_offline(sample, params)
    r_rt = run_realtime(sample, params, CKPT_DIR)
    r_re = run_resume(sample, params, CKPT_DIR, stop_after_windows=137)
    consistency = compare_three(r_rt, r_re, r_off)
    report["three_path_consistency"] = consistency
    report["realtime_stats"] = r_rt.stats
    report["decisions"] = [
        {"tick": d.tick, "reason": d.reason, "old": d.old_delay_ms,
         "new": d.new_delay_ms, "gaps": d.gap_count,
         "avg_queue_ms": d.avg_queue_delay_ms, "rationale": d.rationale}
        for d in r_off.decisions]

    # ---- parallel vs serial ---------------------------------------------
    par = {}
    for workers in (1, 2, 4, 8):
        r = run_realtime(sample, params, CKPT_DIR, n_workers=workers,
                         max_pending_frames=8)
        par["workers=%d" % workers] = bool(
            np.array_equal(r.output, r_off.output)
            and [d.key() for d in r.decisions]
            == [d.key() for d in r_off.decisions])
    report["parallel_equals_serial"] = par

    # ---- checkpoint resume from several points ---------------------------
    resume = {}
    for stop in (1, 50, 137, 250):
        r = run_resume(sample, params, CKPT_DIR, stop_after_windows=stop)
        resume["stop=%d" % stop] = bool(np.array_equal(r.output,
                                                       r_off.output))
    report["resume_points"] = resume

    # ---- error-code checks -------------------------------------------------
    check_error_codes(report)

    # ---- exports -------------------------------------------------------------
    np.save(os.path.join(EXPORT_DIR, "output_realtime.npy"),
            r_rt.output.astype(np.float32))
    with open(os.path.join(EXPORT_DIR, "decisions.json"), "w") as fh:
        json.dump(report["decisions"], fh, ensure_ascii=False, indent=1)
    with open(os.path.join(EXPORT_DIR, "acceptance_report.json"), "w") as fh:
        json.dump(report, fh, ensure_ascii=False, indent=1, default=str)

    ok = (consistency["match"]
          and all(par.values()) and all(resume.values())
          and all(v == "OK" for v in report["error_codes"].values()))
    report["verdict"] = "PASS" if ok else "FAIL"

    lines = [
        "# 验收报告 - 自适应抖动缓冲工作台",
        "",
        "- 样本: %s (seed=%s, %d 帧, %d 条到达记录)" % (
            report["sample"]["id"], report["sample"]["seed"],
            report["sample"]["frames"], report["sample"]["arrivals"]),
        "- 抖动模式: %s" % ",".join(report["sample"]["jitter_patterns"]),
        "- 内容哈希: %s" % report["content_hash"][:16],
        "- 策略版本: %s" % report["policy_version"],
        "",
        "## 三路径一致性",
        "- 实时接收 vs 离线重放 vs 断点恢复: %s" % (
            "逐样本一致" if consistency["match"] else "不一致"),
        "- 提交窗口: %d, 断音: %d, 平均排队: %.1fms, 最终目标延迟: %dms"
        % (r_rt.stats["committed_windows"], r_rt.stats["gap_count"],
           r_rt.stats["avg_queue_delay_ms"],
           r_rt.stats["final_target_delay_ms"]),
        "- 重复帧丢弃: %d, 迟到帧丢弃: %d" % (r_rt.stats["dup_dropped"],
                                              r_rt.stats["late_dropped"]),
        "",
        "## 并发与串行一致性",
    ]
    lines += ["- %s: %s" % (k, "一致" if v else "不一致")
              for k, v in par.items()]
    lines += ["", "## 断点恢复"] + [
        "- %s: %s" % (k, "一致" if v else "不一致")
        for k, v in resume.items()]
    lines += ["", "## 错误码链路"] + [
        "- %s: %s" % kv for kv in report["error_codes"].items()]
    lines += ["", "## 目标延迟决策序列"] + [
        "- t=%dms %s %d->%dms (%s)" % (d["tick"], d["reason"], d["old"],
                                       d["new"], d["rationale"])
        for d in report["decisions"]]
    lines += ["", "## 结论", "- **%s**" % report["verdict"]]
    with open(os.path.join(EXPORT_DIR, "acceptance_report.md"), "w") as fh:
        fh.write("\n".join(lines) + "\n")

    print("\n".join(lines[:32]))
    print("...")
    print("verdict:", report["verdict"])
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
