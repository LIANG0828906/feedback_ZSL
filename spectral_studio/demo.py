"""Scripted demonstration entry point.

Runs a deterministic operation sequence against the local samples:
import + initial publish, white point change (all materials affected),
single-material edit (only that material affected), observer/adaptation
change, rollback, and a simulated forced kill followed by snapshot
recovery.  Writes a trace log containing version numbers, dependency
factors, affected version sets, reused/recomputed counts, encoded values
and content hashes.  The same seed and operation sequence produce a
byte-identical trace log.
"""

from __future__ import annotations

import argparse
import os

from . import samplegen
from .canon import canonical_json
from .engine import MaterialLibrary
from .persistence import save_snapshot
from .versioning import GENESIS_HASH, MaterialVersion, Release, STATUS_INCOMPLETE


def _simulate_forced_kill(library, snapshot_path, target_mid):
    """Write a crash journal as if the process died mid-recompute: the
    target material's next version is present but incomplete."""
    release = library.current_release
    old_v = release.versions[target_mid]
    tentative = dict(release.versions)
    tentative[target_mid] = MaterialVersion(
        material_id=target_mid,
        version=old_v.version + 1,
        params=old_v.params,
        dep_factors=library.env.dependency_factors(),
        encoded={},
        intermediates={},
        prev_hash=old_v.content_hash,
        status=STATUS_INCOMPLETE,
        recompute_reason="material-params-changed",
    )
    journal = Release(release.release_id + 1, tentative, release.env_config,
                      release.ui_state, release.chain_hash)
    library._history[target_mid].append(tentative[target_mid])
    save_snapshot(library, snapshot_path, release=journal)
    library._history[target_mid].pop()


def run_demo(seed=samplegen.DEFAULT_SEED, samples_dir="samples",
             artifacts_dir="artifacts"):
    samplegen.write_samples(samples_dir, seed=seed)
    materials = samplegen.load_samples(samples_dir)
    os.makedirs(artifacts_dir, exist_ok=True)
    snapshot_path = os.path.join(artifacts_dir, f"snapshot_seed{seed}.json")

    library = MaterialLibrary(snapshot_path=snapshot_path)
    ops = [{"op": "add_material", "doc": doc} for doc in materials]
    library.submit(ops, ui_state={"selected_material": materials[0]["id"],
                                  "panel": "materials"})

    # White point change: every material is affected.
    library.submit([{"op": "set_env", "white_point": "D50"}],
                   ui_state={"panel": "whitepoint"})

    # Single material edit: only that material is affected.
    spectral_doc = next(m for m in materials if m["kind"] == "spectral")
    edited = dict(spectral_doc["spectral"])
    edited["reflectance"] = [min(1.0, v * 1.05)
                             for v in edited["reflectance"]]
    library.submit([{"op": "set_material", "id": spectral_doc["id"],
                     "params": {"id": spectral_doc["id"], "kind": "spectral",
                                "spectral": edited}}])

    # Observer + adaptation strategy change: every material is affected.
    library.submit([{"op": "set_env", "observer": "1964_10",
                     "adaptation": "von_kries"}])

    # Rollback the edited material to its previous version (appends a new
    # version; history is not rewritten).
    library.submit([{"op": "rollback", "id": spectral_doc["id"],
                     "version": 3}])

    # Simulated forced kill + recovery from the on-disk snapshot.
    _simulate_forced_kill(library, snapshot_path, materials[1]["id"])
    recovered = MaterialLibrary.recover(snapshot_path)
    recovered_again = MaterialLibrary.recover(snapshot_path)
    recovery_consistent = (recovered.current_release.chain_hash
                           == recovered_again.current_release.chain_hash)

    trace = library.trace + recovered.trace
    trace_path = os.path.join(artifacts_dir, f"trace_seed{seed}.json")
    with open(trace_path, "w", encoding="utf-8") as handle:
        handle.write(canonical_json({
            "seed": seed,
            "entries": trace,
            "final_chain_hash": recovered.current_release.chain_hash,
            "recovery_consistent": recovery_consistent,
        }))
    return {
        "trace_path": trace_path,
        "snapshot_path": snapshot_path,
        "final_chain_hash": recovered.current_release.chain_hash,
        "recovery_consistent": recovery_consistent,
        "release_id": recovered.current_release.release_id,
        "materials": sorted(recovered.current_release.versions),
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description="scripted demo")
    parser.add_argument("--seed", type=int, default=samplegen.DEFAULT_SEED)
    parser.add_argument("--samples-dir", default="samples")
    parser.add_argument("--artifacts-dir", default="artifacts")
    args = parser.parse_args(argv)
    summary = run_demo(seed=args.seed, samples_dir=args.samples_dir,
                       artifacts_dir=args.artifacts_dir)
    for key, value in summary.items():
        print(f"{key}: {value}")


if __name__ == "__main__":
    main()
