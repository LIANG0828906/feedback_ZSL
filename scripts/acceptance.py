"""Scripted acceptance entry point.

Verifies, using only local deterministic samples:
  1. environment (Python >= 3.10, NumPy >= 1.24)
  2. deterministic sample generation (same seed -> byte-identical suite)
  3. v1/v2/v3 parsing and dual-path retargeting runs
  4. fault samples -> expected business error codes
  5. fixed-path deviation within the baseline bound
  6. byte-identical reproducibility of poses and logs
  7. SIGKILL checkpoint recovery (resume + finalize == uninterrupted run)
  8. hash-chain tamper detection (E_RECOVERY_INVALID)
  9. unit test suite

Exit code 0 when every check passes, 1 otherwise.
"""

import json
import os
import shutil
import signal
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PY = sys.executable
ART = os.path.join(ROOT, "artifacts", "acceptance")
SEED = 20261122

_checks = []


def check(name, ok, detail=""):
    _checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name}"
          + (f" -- {detail}" if detail else ""))
    return ok


def cli(*args, expect=0):
    proc = subprocess.run([PY, "-m", "retarget.cli", *args],
                          cwd=ROOT, capture_output=True, text=True)
    out = proc.stdout.strip()
    if expect is not None and proc.returncode != expect:
        return None, f"exit={proc.returncode} stderr={proc.stderr[-300:]}"
    return (json.loads(out) if out else {}), None


def tree_hash_identical(a, b):
    for root, _, files in os.walk(a):
        for f in files:
            pa = os.path.join(root, f)
            pb = pa.replace(a, b, 1)
            if not os.path.exists(pb):
                return False, f"missing {pb}"
            with open(pa, "rb") as fa, open(pb, "rb") as fb:
                if fa.read() != fb.read():
                    return False, f"diff {pa}"
    return True, ""


def main():
    shutil.rmtree(ART, ignore_errors=True)
    os.makedirs(ART, exist_ok=True)

    import numpy
    check("env.python>=3.10", sys.version_info >= (3, 10),
          sys.version.split()[0])
    numpy_version = tuple(int(x)
                          for x in numpy.__version__.split(".")[:2])
    check("env.numpy>=1.24", numpy_version >= (1, 24), numpy.__version__)

    out, err = cli("generate", "--out", os.path.join(ART, "samples"),
                   "--seed", str(SEED))
    check("generate.suite", err is None and out.get("status") == "ok",
          err or f"{out['samples']} samples")
    out2, err = cli("generate", "--out", os.path.join(ART, "samples2"),
                    "--seed", str(SEED))
    same, detail = tree_hash_identical(os.path.join(ART, "samples"),
                                       os.path.join(ART, "samples2"))
    check("generate.deterministic", err is None and same, detail)

    samples = os.path.join(ART, "samples")
    baseline = json.load(open(os.path.join(samples,
                                           "baseline_summary.json")))
    metas = {}
    for name in os.listdir(samples):
        meta_path = os.path.join(samples, name, "meta.json")
        if os.path.exists(meta_path):
            metas[name] = json.load(open(meta_path))

    runs = os.path.join(ART, "runs")
    for name, meta in sorted(metas.items()):
        if meta["fault"]:
            continue
        out_dir = os.path.join(runs, name)
        out, err = cli("run", "--anim", f"{samples}/{name}/anim.json",
                       "--target", f"{samples}/{name}/target.json",
                       "--out", out_dir)
        ok = err is None and out.get("status") == "ok"
        check(f"run.{name}", ok, err or "")
        if not ok:
            continue
        report = json.load(open(os.path.join(out_dir, "report.json")))
        verdicts = {p: v["verdict"] for p, v in report["paths"].items()}
        check(f"run.{name}.verdicts",
              all(v == "pass" for v in verdicts.values()), str(verdicts))
        base = next(s for s in baseline["samples"] if s["name"] == name)
        fixed_max = report["paths"]["fixed"]["max_deviation"]["deg"]
        check(f"run.{name}.fixed_within_baseline",
              fixed_max <= base["predicted_fixed_max_deviation_deg"],
              f"{fixed_max:.5f} <= "
              f"{base['predicted_fixed_max_deviation_deg']:.5f}")
        check(f"run.{name}.float_exact",
              report["paths"]["float"]["max_deviation"]["deg"] < 1e-4,
              f"{report['paths']['float']['max_deviation']['deg']:.2e}")

    for name, meta in sorted(metas.items()):
        if not meta["fault"] or meta["fault"] == "alias_miss":
            continue
        out_dir = os.path.join(runs, name)
        out, err = cli("run", "--anim", f"{samples}/{name}/anim.json",
                       "--target", f"{samples}/{name}/target.json",
                       "--out", out_dir, *meta["run_flags"], expect=3)
        ok = err is None and out and out.get("error") == meta["expect_error"]
        detail = (err or json.dumps(out, sort_keys=True))[:200]
        check(f"fault.{name}.{meta['expect_error']}", bool(ok), detail)
        if meta["expect_error"] == "E_ANIM_INVALID":
            check(f"fault.{name}.no_artifacts",
                  not os.path.exists(out_dir), "no intermediate artifacts")
        if meta["expect_error"] == "E_SCALE_APPLIED":
            check(f"fault.{name}.no_pose_output",
                  not os.path.exists(os.path.join(out_dir,
                                                  "poses_float.jsonl")),
                  "path aborted before pose output")

    out, err = cli("run", "--anim", f"{samples}/fault_alias_miss/anim.json",
                   "--target", f"{samples}/fault_alias_miss/target.json",
                   "--out", os.path.join(runs, "alias_miss_ok"))
    mrep = json.load(open(os.path.join(runs, "alias_miss_ok",
                                       "mapping_report.json")))
    miss = [u for u in mrep["unmatched"] if u["reason"] == "alias_miss"]
    check("mapping.alias_miss_reported",
          len(miss) == 1 and bool(miss[0]["ancestor_candidates"]),
          json.dumps(miss, sort_keys=True)[:160])
    mapped_sources = {m["source"] for m in mrep["mappings"]}
    check("mapping.alias_miss_no_fallback",
          bool(miss) and miss[0]["source"] not in mapped_sources,
          "missed joint stays unmapped")

    victim = os.path.join(samples, "valid_v2_j12_f120")
    cli("run", "--anim", f"{victim}/anim.json", "--target",
        f"{victim}/target.json", "--out", os.path.join(runs, "det_a"))
    cli("run", "--anim", f"{victim}/anim.json", "--target",
        f"{victim}/target.json", "--out", os.path.join(runs, "det_b"))
    same, detail = tree_hash_identical(os.path.join(runs, "det_a"),
                                       os.path.join(runs, "det_b"))
    check("repro.byte_identical", same, detail)

    victim = os.path.join(samples, "valid_v3_j18_f240")
    ref = os.path.join(runs, "kill_ref")
    killed = os.path.join(runs, "kill_resume")
    cli("run", "--anim", f"{victim}/anim.json", "--target",
        f"{victim}/target.json", "--out", ref, "--interval", "16")
    proc = subprocess.Popen(
        [PY, "-m", "retarget.cli", "run", "--anim", f"{victim}/anim.json",
         "--target", f"{victim}/target.json", "--out", killed,
         "--interval", "16", "--interval-delay-ms", "120"],
        cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(0.6)
    proc.send_signal(signal.SIGKILL)
    proc.wait()
    manifest = json.load(open(os.path.join(killed, "ckpt", "manifest.json")))
    partial = 0 < manifest["committed_intervals"] and \
        not manifest["complete"]
    check("recovery.partial_commit", partial,
          f"committed_intervals={manifest['committed_intervals']}")
    out, err = cli("resume", "--out", killed)
    resumed = err is None and out.get("status") == "ok"
    check("recovery.resume", resumed, err or str(out))
    check("recovery.no_exit_report",
          resumed and not os.path.exists(os.path.join(killed,
                                                      "report.json")),
          "resume does not write the exit report")
    out, err = cli("finalize", "--out", killed)
    same, detail = tree_hash_identical(ref, killed)
    check("recovery.finalize_byte_identical", err is None and same, detail)

    tampered = os.path.join(runs, "tampered")
    shutil.copytree(ref, tampered)
    p = os.path.join(tampered, "ckpt", "interval_000002.json")
    doc = json.load(open(p))
    doc["poses"]["float"][0]["global_rot"]["Pelvis"][1] += 0.25
    with open(p, "w") as fh:
        json.dump(doc, fh)
    out, err = cli("resume", "--out", tampered, expect=3)
    check("recovery.tamper_detected",
          err is None and out.get("error") == "E_RECOVERY_INVALID"
          and out["details"]["first_broken_frame"] == 32,
          json.dumps(out, sort_keys=True)[:160])

    proc = subprocess.run([PY, "-m", "unittest", "discover", "-s", "tests"],
                          cwd=ROOT, capture_output=True, text=True)
    tail = proc.stderr.strip().splitlines()
    check("unittest.suite", proc.returncode == 0,
          tail[-1] if tail else "")

    failed = [c for c in _checks if not c[1]]
    print(f"\n{len(_checks) - len(failed)}/{len(_checks)} checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
