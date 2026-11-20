from __future__ import annotations

from dataclasses import replace
from pathlib import Path
from typing import Any

from .canonical import sha256_bytes, write_json_atomic
from .color import WHITE_POINTS
from .library import MaterialLibrary, TransitionResult, load_library
from .models import IlluminantSpec, TransitionRequest, thaw_value
from .samples import GeneratedSamples, generate_samples, material_descriptor_from_params


def _trace_operation(result: TransitionResult) -> dict[str, Any]:
    return {
        "operation": result.operation,
        "generation": result.generation,
        "previous_generation": result.previous_generation,
        "affected_material_ids": list(result.affected_material_ids),
        "affected_versions": dict(result.affected_versions),
        "reused_versions": dict(result.reused_versions),
        "recomputed_count": result.recomputed_count,
        "reused_count": result.reused_count,
        "max_channel_deltas": thaw_value(result.max_channel_deltas),
        "publication_hash": result.publication_hash,
        "snapshot_hash": result.snapshot_hash,
    }


def _edited_oxide_params(library: MaterialLibrary) -> dict[str, Any]:
    material_id = "spectral-oxide-red"
    params = library.current_snapshot.current_record(material_id).params
    descriptor = material_descriptor_from_params(params)
    spectrum = descriptor["spectrum"]
    spectrum["values"] = [
        round(max(0.0, min(1.0, float(value) * 0.88)), 8)
        for value in spectrum["values"]
    ]
    descriptor["truth_source"] = "deterministic-material-edit"
    return dict(library.material_update_params(material_id, descriptor))


def run_demo(seed: int, out_dir: str | Path) -> dict[str, Any]:
    out = Path(out_dir)
    generated = generate_samples(out, seed)
    snapshot_path = out / "state" / "snapshot.json"
    library, pending = load_library(
        snapshot_path,
        trace_path=out / "trace.jsonl",
        pending_path=out / "state" / "pending.json",
    )
    if pending is not None:
        raise RuntimeError("fresh demo unexpectedly has a pending journal")
    initial_snapshot = library.current_snapshot
    initial_generation = initial_snapshot.generation
    operations: list[dict[str, Any]] = []

    config = initial_snapshot.pipeline
    config_5000 = replace(
        config,
        illuminant=IlluminantSpec(
            name="Blackbody-5000K",
            temperature_k=5000.0,
            wavelengths_nm=None,
            values=None,
        ),
    )
    operations.append(
        _trace_operation(
            library.run_transition_sync(
                TransitionRequest(
                    operation="change_illuminant_to_5000k",
                    pipeline=config_5000,
                )
            )
        )
    )

    config_d50 = replace(
        config_5000,
        white_point_name="D50",
        white_point_xyz=WHITE_POINTS["D50"],
    )
    operations.append(
        _trace_operation(
            library.run_transition_sync(
                TransitionRequest(
                    operation="change_white_point_to_d50",
                    pipeline=config_d50,
                )
            )
        )
    )

    config_von_kries = replace(config_d50, adaptation="von-kries")
    operations.append(
        _trace_operation(
            library.run_transition_sync(
                TransitionRequest(
                    operation="change_adaptation_to_von_kries",
                    pipeline=config_von_kries,
                )
            )
        )
    )

    config_10deg = replace(config_von_kries, observer="10deg")
    operations.append(
        _trace_operation(
            library.run_transition_sync(
                TransitionRequest(
                    operation="change_observer_to_10deg",
                    pipeline=config_10deg,
                )
            )
        )
    )

    oxide_params = _edited_oxide_params(library)
    operations.append(
        _trace_operation(
            library.run_transition_sync(
                TransitionRequest(
                    operation="edit_single_spectral_material",
                    pipeline=config_10deg,
                    material_update={"spectral-oxide-red": oxide_params},
                )
            )
        )
    )

    current_oxide = library.current_snapshot.current_record("spectral-oxide-red")
    operations.append(
        _trace_operation(
            library.run_transition_sync(
                TransitionRequest(
                    operation="rollback_to_version_1",
                    pipeline=config_10deg,
                    rollback={
                        "material_id": "spectral-oxide-red",
                        "target_version": 1,
                        "expected_head_hash": current_oxide.content_hash,
                    },
                )
            )
        )
    )

    final_snapshot = library.current_snapshot
    final_snapshot_hash = sha256_bytes(snapshot_path.read_bytes())
    final_publication_hash = final_snapshot.publication_hash
    final_materials = final_snapshot.current_materials()
    final_ui_state = thaw_value(final_snapshot.ui_state)
    library.close()

    restored, restored_pending = load_library(
        snapshot_path,
        trace_path=out / "trace.jsonl",
        pending_path=out / "state" / "pending.json",
    )
    restored_snapshot = restored.current_snapshot
    restored_matches = (
        restored_snapshot.generation == final_snapshot.generation
        and restored_snapshot.publication_hash == final_publication_hash
        and restored_snapshot.current_materials() == final_materials
    )
    restored_ui_state = thaw_value(restored_snapshot.ui_state)
    restored.close()
    summary = {
        "schema": "spectral-material-demo/v1",
        "library_id": generated.library_id,
        "seed": seed,
        "initial_generation": initial_generation,
        "final_generation": final_snapshot.generation,
        "operations": operations,
        "final_materials": final_materials,
        "final_publication_hash": final_publication_hash,
        "final_snapshot_hash": final_snapshot_hash,
        "restored": {
            "generation": restored_snapshot.generation,
            "publication_hash": restored_snapshot.publication_hash,
            "matches_published": restored_matches,
            "pending_job": restored_pending is not None,
            "ui_state": restored_ui_state,
        },
    }
    write_json_atomic(out / "summary.json", summary)
    return summary


def _read_bytes(path: Path) -> bytes:
    return path.read_bytes()


def verify_determinism(seed: int, out_dir: str | Path) -> dict[str, Any]:
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    run_a = out / "run-a"
    run_b = out / "run-b"
    for run_dir in (run_a, run_b):
        if run_dir.exists():
            import shutil

            shutil.rmtree(run_dir)
        run_demo(seed, run_dir)
    relative_files = (
        "materials.json",
        "observer_2deg.json",
        "observer_10deg.json",
        "illuminant_d65.json",
        "baseline_hashes.json",
        "trace.jsonl",
        "summary.json",
        "state/snapshot.json",
        "state/recovery_anchor.json",
    )
    files: dict[str, Any] = {}
    byte_identical = True
    for relative in relative_files:
        data_a = _read_bytes(run_a / relative)
        data_b = _read_bytes(run_b / relative)
        equal = data_a == data_b
        byte_identical = byte_identical and equal
        files[relative] = {
            "equal": equal,
            "sha256": sha256_bytes(data_a),
        }
    result = {
        "schema": "spectral-material-determinism/v1",
        "seed": seed,
        "byte_identical": byte_identical,
        "files": files,
    }
    write_json_atomic(out / "determinism.json", result)
    return result
