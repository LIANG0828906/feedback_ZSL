"""End-to-end acceptance driver for the local deterministic corpus.

Runs the full pipeline without containers or network access and writes every
report under ``artifacts/``:

1. deterministic sample generation (WAV PCM + MP3, plus OGG for one group);
2. parallel index build with checkpoints;
3. similarity search with family merging;
4. ground-truth + baseline verification;
5. legacy upgrade, compatibility matrix, coexistence dedup, monotonicity.
"""

from __future__ import annotations

import json
import os
import shutil
import sys

from afpipe.builder import IndexBuildManager
from afpipe.compat import (
    build_legacy_and_upgrade,
    compatibility_matrix,
    cross_version_family_duplication_check,
    quantisation_monotonicity,
)
from afpipe.config import Config
from afpipe.hashing import atomic_write_bytes, canonical_bytes
from afpipe.indexstore import IndexStore
from afpipe.manifest import load_manifest
from afpipe.samples import DEFAULT_SEED, generate
from afpipe.search import Searcher
from afpipe.verify import (
    run_queries_from_persistence,
    verify_against_baseline,
    verify_against_ground_truth,
)

ROOT = os.path.dirname(os.path.abspath(__file__))
CORPUS = os.path.join(ROOT, "workspace", "acceptance_corpus")
WORK = os.path.join(ROOT, "workspace", "acceptance_work")
INDEX = os.path.join(ROOT, "workspace", "acceptance_index")
ART = os.path.join(ROOT, "artifacts")


def write(name: str, payload) -> str:
    path = os.path.join(ART, name)
    atomic_write_bytes(path, canonical_bytes(payload))
    return path


def main() -> int:
    os.makedirs(ART, exist_ok=True)
    shutil.rmtree(CORPUS, ignore_errors=True)
    shutil.rmtree(WORK, ignore_errors=True)
    shutil.rmtree(INDEX, ignore_errors=True)

    cfg = Config(max_workers=3).validate()
    summary: dict = {"steps": []}

    manifest_raw = generate(CORPUS, seed=DEFAULT_SEED)
    manifest_path = os.path.join(CORPUS, "manifest.json")
    manifest = load_manifest(manifest_path)
    write("acc_01_manifest_fingerprint.json", {
        "manifest_fingerprint": manifest.fingerprint,
        "seed": manifest.seed,
        "file_count": len(manifest.files),
        "groups": list(manifest.groups),
    })
    summary["steps"].append("generate")

    report = IndexBuildManager(WORK, INDEX, cfg, manifest).build()
    write("acc_02_build_report.json", report.to_dict())
    identity = IndexStore(INDEX).identity()
    write("acc_03_index_identity.json", identity)
    assert not report.failed_files, report.failed_files
    summary["steps"].append("build")

    store = IndexStore(INDEX).load(
        cfg, manifest_fingerprint=manifest.fingerprint)
    searcher = Searcher(store, cfg, manifest)
    search_results = []
    for qid, query in sorted(manifest.queries_meta.items()):
        owned = [m for m in manifest.query_files()
                 if m.file_id in (f["file_id"] for f in query["files"])]
        media = next((m for m in owned if m.format_kind == "lossy"),
                     owned[0])
        search_results.append(searcher.query(media, query_id=qid).to_dict())
    write("acc_04_search_results.json", search_results)
    summary["steps"].append("search")

    gt = verify_against_ground_truth(manifest_path, INDEX, cfg)
    write("acc_05_ground_truth_verification.json", gt.to_dict())
    assert gt.ok, gt.to_dict()

    rerun = run_queries_from_persistence(manifest_path, INDEX, cfg)
    baseline_path = write(
        "acc_06_baseline_results.json",
        [r.to_dict() for r in rerun],
    )
    bl = verify_against_baseline(
        manifest_path, INDEX, baseline_path, cfg)
    write("acc_07_baseline_verification.json", bl.to_dict())
    assert bl.ok, bl.to_dict()
    summary["steps"].append("verify")

    upgrade = build_legacy_and_upgrade(WORK, cfg, manifest)
    matrix = compatibility_matrix(
        manifest_path, INDEX, upgrade["upgraded_dir"],
        upgrade["legacy_dir"], cfg,
    )
    mixed_dir = os.path.join(WORK, "index_mixed_v2")
    from afpipe.cli import _build_mixed_store

    _build_mixed_store(
        type("Args", (), {"work": WORK})(),
        cfg, manifest, mixed_dir, upgrade["legacy_dir"],
    )
    dedup = cross_version_family_duplication_check(
        manifest_path, mixed_dir, cfg)
    monotonic = quantisation_monotonicity(manifest_path, cfg)
    write("acc_08_upgrade_trail.json", upgrade["trail"])
    write("acc_09_compatibility_matrix.json", matrix)
    write("acc_10_coexistence_dedup.json", dedup)
    write("acc_11_quantisation_monotonicity.json", monotonic)
    assert matrix["summary"]["unexpected"] == 0
    assert all(row["duplicate_physical_keys"] == 0
               for row in dedup["rows"])
    assert all(p["monotonic_non_increasing"]
               and p["meets_floor_at_default"]
               for p in monotonic["pairs"])
    summary["steps"].append("compat")

    summary["status"] = "PASS"
    summary["search_family_preview"] = [
        {
            "query_id": r["query_id"],
            "top_family": r["families"][0]["physical_key"]
            if r["families"] else None,
            "member_count": len(r["families"][0]["members"])
            if r["families"] else 0,
            "confidence": r["families"][0]["confidence"]
            if r["families"] else None,
        }
        for r in search_results
    ]
    write("acc_00_summary.json", summary)
    print(json.dumps(summary, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
