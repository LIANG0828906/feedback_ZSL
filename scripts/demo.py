#!/usr/bin/env python3
"""Scripted end-to-end demo: deterministic corpus -> baselines -> library ->
pre/post-migration retrieval -> consistency report -> trace log.

All artifacts land in artifacts/demo/. Re-running with the same seed
produces a byte-identical trace log.
"""

import json
import os
import shutil
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from fpworkbench.consistency import compare_results, run_queries
from fpworkbench.errors import (E_BASELINE_MISMATCH, E_BUSY, E_FMT_INVALID,
                                E_LIB_MISMATCH, FpError)
from fpworkbench.extract import extract_fingerprint
from fpworkbench.formats import load_fingerprint, save_fingerprint
from fpworkbench.library import Library
from fpworkbench.samplegen import generate_corpus
from fpworkbench.trace import TraceLog

SEED = 42
TOLERANCE_MS = 64.0  # declared tolerance: 2 quantization units
ROOT = os.path.join(os.path.dirname(__file__), "..", "artifacts", "demo")


def main():
    root = os.path.abspath(ROOT)
    if os.path.isdir(root):
        shutil.rmtree(root)
    os.makedirs(root)
    trace = TraceLog()
    trace.event("demo_start", seed=SEED, tolerance_ms=TOLERANCE_MS)

    # 1. deterministic corpus + v1/v2/v3 baselines
    manifest = generate_corpus(root, seed=SEED)
    for s in manifest["samples"]:
        trace.event("sample", sample_id=s["sample_id"],
                    sample_rate=s["sample_rate"], channels=s["channels"],
                    version=s["version"], baseline=s["baseline"])

    # 2. build the library from the mixed-version baselines
    lib = Library(root)
    for s in manifest["samples"]:
        entry = lib.add_fingerprint_file(os.path.join(root, s["baseline"]))
        trace.event("library_add", sample_id=entry["sample_id"],
                    version=entry["version"], sha256=entry["sha256"])
    lib.save()
    trace.event("library_state", distribution=lib.version_distribution())

    # 3. extract query fingerprints
    queries = []
    for q in manifest["queries"]:
        fp = extract_fingerprint(os.path.join(root, q["wav"]), q["query_id"])
        queries.append(fp)
        trace.event("query_extract", query_id=q["query_id"],
                    hashes=len(fp.hashes),
                    truth_offset_ms=q["truth_offset_ms"],
                    source_sample=q["source_sample"])

    # 4. pre-migration retrieval (mixed v1/v2/v3 library)
    before = run_queries(queries, lib.load_fingerprints())
    for qid, cands in before.items():
        for c in cands:
            trace.event("match_pre", query=qid, sample=c["sample_id"],
                        offset_ms=c["offset_ms"], votes=c["votes"],
                        confidence=c["confidence"],
                        lib_version=c["lib_version"])

    # 5. migrate everything to v3
    reports = lib.migrate_all(target_version=3)
    for r in reports:
        trace.event("migrate", sample_id=r.sample_id,
                    source_version=r.source_version,
                    target_version=r.target_version, ok=r.ok,
                    mappings=len(r.field_mappings),
                    losses=[l["field"] for l in r.lost_fields])
    trace.event("library_state", distribution=lib.version_distribution())

    # 6. post-migration retrieval + consistency comparison
    after = run_queries(queries, lib.load_fingerprints())
    for qid, cands in after.items():
        for c in cands:
            trace.event("match_post", query=qid, sample=c["sample_id"],
                        offset_ms=c["offset_ms"], votes=c["votes"],
                        confidence=c["confidence"],
                        lib_version=c["lib_version"])
    consistency = compare_results(before, after, TOLERANCE_MS,
                                  before_version="mixed(v1/v2/v3)",
                                  after_version="v3")
    for d in consistency["details"]:
        trace.event("consistency", **d)
    trace.event("consistency_summary",
                passed=consistency["passed"],
                comparisons=consistency["comparisons"],
                ok=consistency["ok"])

    # 7. offset truth check: matched offset vs generator truth
    for q in manifest["queries"]:
        cands = after.get(q["query_id"], [])
        hit = next((c for c in cands
                    if c["sample_id"] == q["source_sample"]), None)
        if hit:
            err = abs(hit["offset_ms"] - q["truth_offset_ms"])
            trace.event("truth_check", query=q["query_id"],
                        truth_offset_ms=q["truth_offset_ms"],
                        matched_offset_ms=hit["offset_ms"],
                        error_ms=round(err, 6),
                        within_tolerance=err <= TOLERANCE_MS)

    # 8. error-code demonstrations (on throwaway copies)
    _demo_error_codes(root, trace)

    # 9. reports + trace log
    with open(os.path.join(root, "migration_report.json"), "w",
              encoding="utf-8") as fh:
        json.dump([r.to_dict() for r in reports], fh, indent=2,
                  sort_keys=True)
        fh.write("\n")
    with open(os.path.join(root, "consistency_report.json"), "w",
              encoding="utf-8") as fh:
        json.dump(consistency, fh, indent=2, sort_keys=True)
        fh.write("\n")
    trace.event("demo_end", passed=consistency["passed"])
    log_path = trace.save(os.path.join(root, "trace.log"))
    print(f"trace log: {log_path}")
    print(f"consistency passed: {consistency['passed']} "
          f"({consistency['ok']}/{consistency['comparisons']} comparisons)")
    return 0 if consistency["passed"] else 1


def _demo_error_codes(root, trace):
    bad_dir = os.path.join(root, "error_cases")
    os.makedirs(bad_dir, exist_ok=True)

    # E_FMT_INVALID: missing field
    fp = extract_fingerprint(os.path.join(root, "wav", "track_alpha.wav"),
                             "tmp")
    p = os.path.join(bad_dir, "missing_field.v1.json")
    save_fingerprint(fp, p, version=1)
    doc = json.load(open(p))
    del doc["payload"]["peak_pairs"]
    from fpworkbench.formats.common import compute_checksum
    doc["checksum"] = compute_checksum(doc["payload"])
    json.dump(doc, open(p, "w"))
    try:
        load_fingerprint(p)
    except FpError as e:
        trace.event("error_case", expect=E_FMT_INVALID, got=e.code,
                    file=os.path.basename(p), field=e.field)

    # E_FMT_INVALID: checksum mismatch
    p2 = os.path.join(bad_dir, "bad_checksum.v1.json")
    save_fingerprint(fp, p2, version=1)
    doc = json.load(open(p2))
    doc["payload"]["sample_rate"] = 12345
    json.dump(doc, open(p2, "w"))
    try:
        load_fingerprint(p2)
    except FpError as e:
        trace.event("error_case", expect=E_FMT_INVALID, got=e.code,
                    file=os.path.basename(p2), field=e.field)

    # E_FMT_INVALID: unknown version
    p3 = os.path.join(bad_dir, "unknown_version.json")
    json.dump({"format_version": 9, "payload": {}, "checksum": "x"},
              open(p3, "w"))
    try:
        load_fingerprint(p3)
    except FpError as e:
        trace.event("error_case", expect=E_FMT_INVALID, got=e.code,
                    file=os.path.basename(p3), field=e.field)

    # E_BASELINE_MISMATCH: v2 fingerprint quantized at 48ms vs unified 32ms
    fp48 = extract_fingerprint(os.path.join(root, "wav", "track_beta.wav"),
                               "track_beta_48")
    fp48.time_quant_ms = 48.0
    fp48.hashes = [(h, round(round(t / 48.0) * 48.0, 6))
                   for h, t in fp48.hashes]
    query = extract_fingerprint(os.path.join(root, "wav", "query_beta.wav"),
                                "q")
    from fpworkbench.retrieve import search
    try:
        search(query, [fp48])
    except FpError as e:
        trace.event("error_case", expect=E_BASELINE_MISMATCH, got=e.code,
                    query_quant_ms=e.detail.get("query_quant_ms"),
                    library_quant_ms=e.detail.get("library_quant_ms"))

    # E_BUSY: concurrent migration on the same library
    lib = Library(root)
    lib.load()
    lib._migrate_lock.acquire()
    try:
        lib.migrate_all()
    except FpError as e:
        trace.event("error_case", expect=E_BUSY, got=e.code)
    finally:
        lib._migrate_lock.release()

    # E_LIB_MISMATCH: tampered manifest content hash
    tampered = json.load(open(lib.manifest_path))
    tampered["content_hash"] = "0" * 64
    json.dump(tampered, open(lib.manifest_path, "w"))
    try:
        Library(root).load()
    except FpError as e:
        trace.event("error_case", expect=E_LIB_MISMATCH, got=e.code,
                    field=e.field)
    lib.save()  # restore a valid manifest


if __name__ == "__main__":
    sys.exit(main())
