"""Scripted experiment: build, query, budget, kill/recovery, consistency."""

import json
import shutil
import subprocess
import sys
from pathlib import Path

from . import samplegen
from .config import load_config, save_config
from .consistency import (compare_fp_hashes, compare_query_results,
                          load_json, write_report)
from .errors import (AlignMismatch, AudioUnsupported, Busy, LibMismatch,
                     RecoveryInvalid)
from .fingerprint import content_hash, diff_summary, fingerprint_signal
from .library import Library
from .scheduler import BuildScheduler
from .search import search
from .tracelog import TraceLog
from .wavio import read_wav


def _tree_hashes(root):
    """Relative path -> sha256 for every file under root (deterministic)."""
    import hashlib
    root = Path(root)
    out = {}
    for p in sorted(root.rglob("*")):
        if p.is_file():
            out[str(p.relative_to(root))] = hashlib.sha256(
                p.read_bytes()).hexdigest()
    return out


def _write_json(path, obj):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(obj, fh, sort_keys=True, indent=1)
        fh.write("\n")


def generate(workdir, seed, n_samples=12, n_queries=4):
    return samplegen.generate(workdir, seed, n_samples, n_queries)


def baseline_hashes(workdir, config):
    """Baseline fingerprint hashes for all generated samples (stage 0)."""
    gt = load_json(Path(workdir) / "ground_truth.json")
    fp_cfg = config["fingerprint"]
    hashes = {}
    for entry in gt["samples"]:
        signal, rate = read_wav(Path(workdir) / entry["path"])
        records, _ = fingerprint_signal(signal, rate, fp_cfg)
        hashes[entry["sample_id"]] = content_hash(records)
    _write_json(Path(workdir) / "baseline_hashes.json", hashes)
    return hashes


def build(workdir, config, batch_id="batch-1", batch_seq=1,
          trace_name="trace_build.jsonl", kill_after=None):
    workdir = Path(workdir)
    lib = Library(workdir / "lib")
    committed = lib.recover()  # disk-state-only recovery; validates chain
    gt = load_json(workdir / "ground_truth.json")
    specs = [(e["sample_id"], str(workdir / e["path"])) for e in gt["samples"]]
    tracer = TraceLog(workdir / "logs" / trace_name)
    tracer.log("BATCH_START", batch_id=batch_id, batch_seq=batch_seq,
               config_version=config["config_version"],
               parallelism=config["scheduler"]["parallelism"],
               per_task_mem_budget=config["scheduler"]["per_task_mem_budget"],
               pool_mem_budget=config["scheduler"]["pool_mem_budget"],
               recovered_committed=len(committed))
    sched = BuildScheduler(config, lib, tracer)
    summary = sched.run_batch(batch_id, batch_seq, specs, kill_after=kill_after)
    tracer.log("BATCH_DONE", batch_id=batch_id, batch_seq=batch_seq, **summary)
    tracer.close()
    return summary


def query(workdir, config, results_name="results.json",
          trace_name="trace_query.jsonl"):
    workdir = Path(workdir)
    lib = Library(workdir / "lib")
    gt = load_json(workdir / "ground_truth.json")
    fp_cfg = config["fingerprint"]
    tracer = TraceLog(workdir / "logs" / trace_name)
    results = []
    for q in gt["queries"]:
        signal, rate = read_wav(workdir / q["path"])
        records, stats = fingerprint_signal(signal, rate, fp_cfg)
        res = search(lib, records, q["query_id"], rate, stats["n_frames"],
                     fp_cfg, config["search"])
        results.append(res)
        tracer.log("QUERY", query_id=q["query_id"],
                   lib_version=res["lib_version"],
                   hits=[{"sample_id": c["sample_id"],
                          "start_frame": c["start_frame"],
                          "votes": c["votes"]} for c in res["candidates"]])
    tracer.close()
    _write_json(workdir / "reports" / results_name, results)
    return results


def check_invalid_audio(workdir):
    gt = load_json(Path(workdir) / "ground_truth.json")
    out = []
    for item in gt["invalid"]:
        try:
            read_wav(Path(workdir) / item["path"])
            out.append({"path": item["path"], "error": None})
        except AudioUnsupported as exc:
            out.append({"path": item["path"], "error": exc.code,
                        "field": exc.field})
    return out


def lib_fp_hashes(workdir):
    lib = Library(Path(workdir) / "lib")
    return {e["sample_id"]: e["fp_hash"]
            for e in lib.load_manifest()["entries"]}


def _variant(config, **sched):
    import copy
    cfg = copy.deepcopy(config)
    cfg["scheduler"].update(sched)
    return cfg


def run_full_experiment(root, seed=20261122, n_samples=12, n_queries=4):
    """Run the whole scripted experiment under `root`; return summary dict."""
    root = Path(root)
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True)
    base_cfg = load_config()
    summary = {"seed": seed, "checks": []}

    def check(name, ok, detail=""):
        summary["checks"].append({"name": name, "ok": bool(ok),
                                  "detail": detail})
        return ok

    # ---- A: default pipeline ---------------------------------------------
    wa = root / "a"
    generate(wa, seed, n_samples, n_queries)
    save_config(base_cfg, wa / "config.json")
    base_hashes = baseline_hashes(wa, base_cfg)
    invalid = check_invalid_audio(wa)
    check("invalid_audio_fields",
          all(i["error"] == "E_AUDIO_UNSUPPORTED" for i in invalid)
          and {i["field"] for i in invalid}
              == {"sample_rate", "bits_per_sample", "channels"},
          json.dumps(invalid, sort_keys=True))
    pre = query(wa, base_cfg, "results_empty.json", "trace_query_pre.jsonl")
    check("pre_build_query_empty",
          all(len(r["candidates"]) == 0 for r in pre))
    build(wa, base_cfg)
    res_a = query(wa, base_cfg)
    check("post_build_query_hits",
          all(len(r["candidates"]) >= 1 for r in res_a),
          f"hits={[len(r['candidates']) for r in res_a]}")
    check("build_hashes_equal_baseline",
          lib_fp_hashes(wa) == base_hashes)

    # E_BUSY: starting a batch while one is active
    lib_a = Library(wa / "lib")
    lib_a.acquire_batch("batch-1")
    try:
        BuildScheduler(base_cfg, lib_a, TraceLog(root / "scratch_busy.jsonl")
                       ).run_batch("batch-1", 1, [])
        check("busy_error", False)
    except Busy:
        check("busy_error", True)
    finally:
        lib_a.release_batch()

    # ---- B: byte-identical reproduction ----------------------------------
    wb = root / "b"
    generate(wb, seed, n_samples, n_queries)
    save_config(base_cfg, wb / "config.json")
    baseline_hashes(wb, base_cfg)
    query(wb, base_cfg, "results_empty.json", "trace_query_pre.jsonl")
    build(wb, base_cfg)
    query(wb, base_cfg)
    ha, hb = _tree_hashes(wa), _tree_hashes(wb)
    diff = sorted(k for k in ha if ha[k] != hb.get(k))
    check("byte_reproducible", not diff and set(ha) == set(hb),
          f"diff_files={diff}")

    # ---- parallelism invariance ------------------------------------------
    wp1 = root / "p1"
    generate(wp1, seed, n_samples, n_queries)
    cfg_p1 = _variant(base_cfg, parallelism=1)
    save_config(cfg_p1, wp1 / "config.json")
    build(wp1, cfg_p1)
    res_p1 = query(wp1, cfg_p1)
    la, lp1 = _tree_hashes(wa / "lib"), _tree_hashes(wp1 / "lib")
    check("parallelism_invariant_lib", la == lp1)
    ok_p1, lines_p1 = compare_query_results(
        res_a, res_p1, base_cfg["search"]["time_tolerance_frames"],
        "parallel4", "parallel1")
    check("parallelism_invariant_query", ok_p1, "; ".join(lines_p1[:3]))

    # ---- degradation (no rejection): hashes must not drift ----------------
    wd = root / "degrade"
    generate(wd, seed, n_samples, n_queries)
    cfg_deg = _variant(base_cfg, per_task_mem_budget=600 * 1024)
    save_config(cfg_deg, wd / "config.json")
    build(wd, cfg_deg)
    res_d = query(wd, cfg_deg)
    degrade_events = [ln for ln in
                      (wd / "logs" / "trace_build.jsonl").read_text().splitlines()
                      if '"TASK_DEGRADE"' in ln]
    check("degrade_stages_logged", len(degrade_events) >= 1,
          f"degrade_events={len(degrade_events)}")
    ok_deg, lines_deg = compare_fp_hashes(lib_fp_hashes(wd), base_hashes,
                                          "degrade", "baseline")
    check("degrade_hashes_stable", ok_deg, "; ".join(lines_deg[:3]))
    ok_q, lines_q = compare_query_results(
        res_a, res_d, base_cfg["search"]["time_tolerance_frames"],
        "default", "degrade")
    check("degrade_query_consistent", ok_q, "; ".join(lines_q[:3]))

    # ---- tight budget: E_BUDGET_EXCEEDED rejection + isolation ------------
    wt = root / "tight"
    generate(wt, seed, n_samples, n_queries)
    cfg_tight = _variant(base_cfg, per_task_mem_budget=250 * 1024)
    save_config(cfg_tight, wt / "config.json")
    summary_t = build(wt, cfg_tight)
    tight_hashes = lib_fp_hashes(wt)
    n_total = len(load_json(wt / "ground_truth.json")["samples"])
    check("budget_rejected_some", summary_t["rejected"] >= 1,
          json.dumps(summary_t))
    check("budget_rejected_absent",
          len(tight_hashes) == summary_t["committed"]
          and summary_t["committed"] + summary_t["rejected"] == n_total)
    bad = [sid for sid, h in tight_hashes.items()
           if base_hashes.get(sid) != h]
    check("tight_committed_hashes_stable", not bad,
          f"mismatched={bad}")
    res_t = query(wt, cfg_tight)
    Library(wt / "lib").verify()  # must not raise

    # ---- pool-budget waiting queue trace ----------------------------------
    wp = root / "poolwait"
    generate(wp, seed, n_samples, n_queries)
    cfg_pool = _variant(base_cfg, pool_mem_budget=5 * 1024 * 1024)
    save_config(cfg_pool, wp / "config.json")
    build(wp, cfg_pool)
    wait_events = [ln for ln in
                   (wp / "logs" / "trace_build.jsonl").read_text().splitlines()
                   if '"TASK_WAIT"' in ln]
    check("pool_wait_logged", len(wait_events) >= 1,
          f"wait_events={len(wait_events)}")
    check("pool_wait_lib_equal", _tree_hashes(wp / "lib")
          == _tree_hashes(wa / "lib"))

    # ---- kill + recovery ---------------------------------------------------
    wk = root / "kill"
    generate(wk, seed, n_samples, n_queries)
    save_config(base_cfg, wk / "config.json")
    kill_at = max(2, n_samples // 2 - 1)
    proc = subprocess.run(
        [sys.executable, "-m", "fpkit.cli", "build", "--workdir", str(wk),
         "--kill-after", str(kill_at)],
        capture_output=True, text=True)
    check("kill_simulated", proc.returncode == 137,
          f"rc={proc.returncode}")
    mid = Library(wk / "lib").recover()
    check("kill_partial_committed", len(mid) == kill_at,
          f"committed={len(mid)}")
    build(wk, base_cfg, trace_name="trace_resume.jsonl")
    check("recovery_lib_equal", _tree_hashes(wk / "lib")
          == _tree_hashes(wa / "lib"))
    res_k = query(wk, base_cfg)
    ok_k, lines_k = compare_query_results(
        res_a, res_k, base_cfg["search"]["time_tolerance_frames"],
        "uninterrupted", "recovered")
    check("recovery_query_consistent", ok_k, "; ".join(lines_k[:3]))

    # duplicate the kill+resume sequence: traces must be byte-identical
    wk2 = root / "kill2"
    generate(wk2, seed, n_samples, n_queries)
    save_config(base_cfg, wk2 / "config.json")
    subprocess.run(
        [sys.executable, "-m", "fpkit.cli", "build", "--workdir", str(wk2),
         "--kill-after", str(kill_at)],
        capture_output=True, text=True)
    build(wk2, base_cfg, trace_name="trace_resume.jsonl")
    check("recovery_trace_reproducible",
          (wk / "logs" / "trace_build.jsonl").read_bytes()
          == (wk2 / "logs" / "trace_build.jsonl").read_bytes()
          and (wk / "logs" / "trace_resume.jsonl").read_bytes()
          == (wk2 / "logs" / "trace_resume.jsonl").read_bytes())

    # ---- E_LIB_MISMATCH / E_RECOVERY_INVALID ------------------------------
    scratch = root / "scratch_libmismatch"
    shutil.copytree(wa / "lib", scratch / "lib")
    rec0 = sorted((scratch / "lib" / "records").glob("*.fp.json"))[0]
    obj = load_json(rec0)
    obj["fp_hash"] = "0" * 64
    _write_json(rec0, obj)
    try:
        Library(scratch / "lib").verify()
        check("lib_mismatch_error", False)
    except LibMismatch as exc:
        check("lib_mismatch_error", True, f"sample={exc.sample_id}")

    scratch2 = root / "scratch_broken_chain"
    shutil.copytree(wa / "lib", scratch2 / "lib")
    man = load_json(scratch2 / "lib" / "manifest.json")
    victim = man["entries"][2]
    victim["fp_hash"] = "f" * 64  # breaks the chain at entry 2
    _write_json(scratch2 / "lib" / "manifest.json", man)
    try:
        Library(scratch2 / "lib").recover()
        check("recovery_invalid_error", False)
    except RecoveryInvalid as exc:
        check("recovery_invalid_error",
              exc.sample_id == victim["sample_id"],
              f"first_broken={exc.sample_id}")

    # ---- E_ALIGN_MISMATCH ---------------------------------------------------
    import copy
    cfg_q8 = copy.deepcopy(base_cfg)
    cfg_q8["fingerprint"]["time_quant"] = 8
    gt = load_json(wa / "ground_truth.json")
    signal, rate = read_wav(wa / gt["queries"][0]["path"])
    qrec, qstats = fingerprint_signal(signal, rate, cfg_q8["fingerprint"])
    try:
        search(Library(wa / "lib"), qrec, "query-00", rate,
               qstats["n_frames"], cfg_q8["fingerprint"], cfg_q8["search"])
        check("align_mismatch_error", False)
    except AlignMismatch:
        check("align_mismatch_error", True)

    # ---- recompute diff summary (window/hop change) ------------------------
    s0 = gt["samples"][0]
    sig, srate = read_wav(wa / s0["path"])
    rec_old, _ = fingerprint_signal(sig, srate, base_cfg["fingerprint"])
    cfg_hop = copy.deepcopy(base_cfg)
    cfg_hop["fingerprint"]["hop_size"] = 256
    rec_new, _ = fingerprint_signal(sig, srate, cfg_hop["fingerprint"])
    diff = diff_summary(rec_old, rec_new)
    check("recompute_diff_summary",
          diff["hash_a"] != diff["hash_b"] and diff["only_in_b"] > 0,
          json.dumps({k: diff[k] for k in
                      ("records_a", "records_b", "common")}))

    # ---- consistency report (per-query/per-candidate, no aggregates only) --
    report_sections = [
        ("parallelism 4 vs 1", lines_p1),
        ("default vs degrade", lines_q),
        ("uninterrupted vs recovered", lines_k),
        ("recompute diff summary", [json.dumps(diff, sort_keys=True)]),
        ("invalid audio", [json.dumps(i, sort_keys=True) for i in invalid]),
    ]
    write_report(root / "reports" / "consistency_report.txt", report_sections)
    summary["all_ok"] = all(c["ok"] for c in summary["checks"])
    _write_json(root / "reports" / "summary.json", summary)
    return summary
