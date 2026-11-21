from __future__ import annotations

import json
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .hashing import canonical_json_bytes
from .model import SurfaceAssembly, parse_assembly, save_assembly
from .runtime import RuntimeUpdate, SurfaceRuntime


@dataclass(frozen=True)
class CaseSpec:
    name: str
    tiles_x: int
    tiles_y: int
    control_rows: int
    control_cols: int
    default_precision: int
    surface_type: str
    precision_overrides: Mapping[int, int]
    flipped_blocks: frozenset[int]
    amplitude: float
    phase: float


CASE_SPECS: tuple[CaseSpec, ...] = (
    CaseSpec(
        name="single_patch",
        tiles_x=1,
        tiles_y=1,
        control_rows=4,
        control_cols=4,
        default_precision=2,
        surface_type="bezier",
        precision_overrides={},
        flipped_blocks=frozenset(),
        amplitude=0.42,
        phase=0.11,
    ),
    CaseSpec(
        name="tiles_2x2",
        tiles_x=2,
        tiles_y=2,
        control_rows=4,
        control_cols=4,
        default_precision=2,
        surface_type="bezier",
        precision_overrides={2: 1},
        flipped_blocks=frozenset({3}),
        amplitude=0.34,
        phase=0.23,
    ),
    CaseSpec(
        name="tiles_3x2",
        tiles_x=3,
        tiles_y=2,
        control_rows=4,
        control_cols=4,
        default_precision=1,
        surface_type="bezier",
        precision_overrides={1: 2, 4: 3},
        flipped_blocks=frozenset({2, 5}),
        amplitude=0.29,
        phase=0.37,
    ),
    CaseSpec(
        name="precision_mix",
        tiles_x=2,
        tiles_y=3,
        control_rows=4,
        control_cols=4,
        default_precision=2,
        surface_type="bspline",
        precision_overrides={0: 0, 2: 1, 3: 3, 5: 4},
        flipped_blocks=frozenset({1}),
        amplitude=0.25,
        phase=0.53,
    ),
)


def _control_lattice(spec: CaseSpec, seed: int) -> np.ndarray:
    control_rows = spec.tiles_y * (spec.control_rows - 1) + 1
    control_cols = spec.tiles_x * (spec.control_cols - 1) + 1
    lattice = np.zeros((control_rows, control_cols, 3), dtype=np.float64)
    for row in range(control_rows):
        for col in range(control_cols):
            u = col / (control_cols - 1)
            v = row / (control_rows - 1)
            x = u
            y = v
            z = (
                spec.amplitude
                * np.sin(np.pi * u)
                * np.sin(np.pi * v)
                * (
                    0.6
                    + 0.4
                    * np.cos(
                        2.0 * np.pi * u
                        + spec.phase
                        + (seed % 17) * 0.013
                    )
                )
            )
            lattice[row, col] = (x, y, z)
    return lattice


def generate_case(spec: CaseSpec, seed: int = 0) -> dict[str, Any]:
    lattice = _control_lattice(spec, seed)
    patches: list[dict[str, Any]] = []
    block_id = 0
    for tile_y in range(spec.tiles_y):
        for tile_x in range(spec.tiles_x):
            row_offset = tile_y * (spec.control_rows - 1)
            col_offset = tile_x * (spec.control_cols - 1)
            control = lattice[
                row_offset : row_offset + spec.control_rows,
                col_offset : col_offset + spec.control_cols,
            ]
            precision = spec.precision_overrides.get(
                block_id, spec.default_precision
            )
            patches.append(
                {
                    "id": f"p{block_id:04d}",
                    "block_id": block_id,
                    "surface_type": spec.surface_type,
                    "precision_level": precision,
                    "flip_normal": block_id in spec.flipped_blocks,
                    "control_points": control.tolist(),
                }
            )
            block_id += 1
    return {
        "schema_version": 1,
        "assembly_id": f"sample-{spec.name}",
        "patches": patches,
    }


def operations_for_case(spec: CaseSpec) -> list[dict[str, Any]]:
    patch_count = spec.tiles_x * spec.tiles_y
    primary = "p0001" if patch_count > 1 else "p0000"
    secondary = "p0002" if patch_count > 2 else "p0000"
    operations: list[dict[str, Any]] = []
    if patch_count == 1:
        alternate_type = "bspline" if spec.surface_type == "bezier" else "bezier"
        operations.append(
            {
                "op": "set_surface_type",
                "patch_id": primary,
                "value": alternate_type,
            }
        )
    operations.extend(
        [
            {
                "op": "control_point_delta",
                "patch_id": primary,
                "row": 1,
                "column": 1,
                "delta": [0.0, 0.0, 0.19],
            },
            {
                "op": "set_precision",
                "patch_id": secondary,
                "value": (
                    spec.precision_overrides.get(
                        2 if patch_count > 2 else 0, spec.default_precision
                    )
                    + 1
                )
                % 5,
            },
            {
                "op": "set_flip_normal",
                "patch_id": "p0000",
                "value": True,
            },
            {
                "op": "control_point_delta",
                "patch_id": primary,
                "row": 1,
                "column": 2 if spec.control_cols > 3 else 1,
                "delta": [0.0, 0.0, -0.07],
            },
        ]
    )
    return operations


def write_cases(root: str | Path, *, seed: int = 0) -> list[Path]:
    output = Path(root)
    if output.exists():
        shutil.rmtree(output)
    output.mkdir(parents=True, exist_ok=True)
    case_index: list[dict[str, Any]] = []
    written: list[Path] = []
    for spec in CASE_SPECS:
        case_dir = output / spec.name
        case_dir.mkdir(parents=True, exist_ok=True)
        assembly_data = generate_case(spec, seed=seed)
        assembly = parse_assembly(assembly_data)
        save_assembly(assembly, case_dir / "assembly.json")
        operations = operations_for_case(spec)
        (case_dir / "operations.json").write_bytes(
            canonical_json_bytes({"operations": operations}) + b"\n"
        )
        case_index.append(
            {
                "name": spec.name,
                "patch_count": len(assembly.patches),
                "control_shape": [
                    spec.control_rows,
                    spec.control_cols,
                ],
                "precision_levels": [
                    patch.precision_level for patch in assembly.patches
                ],
            }
        )
        written.append(case_dir)
    (output / "cases.json").write_bytes(
        canonical_json_bytes({"seed": seed, "cases": case_index}) + b"\n"
    )
    return written


def operation_changes(
    assembly: SurfaceAssembly, operation: Mapping[str, Any]
) -> tuple[str, dict[str, Any]]:
    op = operation.get("op")
    patch_id = operation.get("patch_id")
    if not isinstance(patch_id, str):
        raise ValueError("operation.patch_id must be a string")
    patch = assembly.patch(patch_id)
    if op == "control_point_delta":
        row = int(operation["row"])
        column = int(operation["column"])
        delta = [float(value) for value in operation["delta"]]
        control = np.asarray(patch.control_points, dtype=np.float64)
        if row < 0 or row >= control.shape[0] or column < 0 or column >= control.shape[1]:
            raise ValueError("control point operation is outside the patch grid")
        control[row, column, :3] += np.asarray(delta, dtype=np.float64)
        return patch_id, {"control_points": control.tolist()}
    if op == "set_precision":
        return patch_id, {"precision_level": int(operation["value"])}
    if op == "set_flip_normal":
        return patch_id, {"flip_normal": bool(operation["value"])}
    if op == "set_surface_type":
        return patch_id, {"surface_type": str(operation["value"])}
    raise ValueError(f"unsupported sample operation: {op}")


def trace_event(
    update: RuntimeUpdate,
    *,
    case_name: str,
    operation_index: int,
    operation: Mapping[str, Any] | None,
) -> dict[str, Any]:
    fields = update.result.to_trace_fields()
    parameters = {
        patch.patch_id: patch.parameter_versions
        for patch in sorted(
            update.result.assembly.patches, key=lambda patch: patch.patch_id
        )
    }
    event = {
        "trace_version": 1,
        "case": case_name,
        "assembly_id": update.result.assembly.assembly_id,
        "operation_index": operation_index,
        "operation": dict(operation) if operation is not None else {"op": "bootstrap"},
        "commit_seq": update.commit.commit_seq,
        "commit_chain_head": update.commit.chain_head,
        "previous_commit_chain_head": update.commit.previous_chain_head,
        "patch_parameter_versions": parameters,
        "patch_versions": {
            patch.patch_id: patch.version_hash
            for patch in sorted(
                update.result.assembly.patches, key=lambda patch: patch.patch_id
            )
        },
        **fields,
    }
    return event


def write_baseline(
    samples_root: str | Path,
    baseline_root: str | Path,
    *,
    preview_backend: str = "software",
    verify_full: bool = True,
) -> dict[str, Any]:
    samples = Path(samples_root)
    baseline = Path(baseline_root)
    if baseline.exists():
        shutil.rmtree(baseline)
    baseline.mkdir(parents=True, exist_ok=True)
    case_index = json.loads(
        (samples / "cases.json").read_text(encoding="utf-8")
    )
    summary: dict[str, Any] = {
        "trace_version": 1,
        "seed": case_index.get("seed", 0),
        "cases": {},
    }

    for case_dir in sorted(path for path in samples.iterdir() if path.is_dir()):
        assembly = parse_assembly(
            json.loads((case_dir / "assembly.json").read_text(encoding="utf-8"))
        )
        operations = json.loads(
            (case_dir / "operations.json").read_text(encoding="utf-8")
        )["operations"]
        case_output = baseline / case_dir.name
        store_dir = case_output / "store"
        events: list[dict[str, Any]] = []

        def capture(update: RuntimeUpdate) -> None:
            events.append(update)

        runtime = SurfaceRuntime.bootstrap(
            assembly,
            store_dir,
            preview_backend=preview_backend,
            verify_full=verify_full,
            trace_callback=capture,
        )
        action_records: list[dict[str, Any]] = []
        initial = events.pop(0)
        action_records.append(
            {
                "operation_index": 0,
                "operation": "bootstrap",
                "commit_seq": initial.commit.commit_seq,
                "assembly_version": initial.result.assembly.version_hash,
                "screen_preview_hash": initial.result.screen_preview.content_hash,
                "chain_head": initial.commit.chain_head,
            }
        )

        for operation_index, operation in enumerate(operations, start=1):
            patch_id, changes = operation_changes(runtime.engine.assembly, operation)
            update = runtime.update(
                patch_id, changes, verify_full=verify_full
            )
            action_records.append(
                {
                    "operation_index": operation_index,
                    "operation": operation,
                    "commit_seq": update.commit.commit_seq,
                    "assembly_version": update.result.assembly.version_hash,
                    "screen_preview_hash": update.result.screen_preview.content_hash,
                    "affected_artifacts": list(
                        update.result.invalidations.affected_ids
                    ),
                    "reused_count": len(update.result.reused_ids),
                    "recomputed_count": len(update.result.recomputed_ids),
                    "chain_head": update.commit.chain_head,
                }
            )

        initial_event = trace_event(
            initial,
            case_name=case_dir.name,
            operation_index=0,
            operation=None,
        )
        trace_path = case_output / "trace.jsonl"
        with trace_path.open("wb") as stream:
            stream.write(canonical_json_bytes(initial_event) + b"\n")
            for operation_index, (update, operation) in enumerate(
                zip(events, operations, strict=True), start=1
            ):
                event = trace_event(
                    update,
                    case_name=case_dir.name,
                    operation_index=operation_index,
                    operation=operation,
                )
                stream.write(canonical_json_bytes(event) + b"\n")

        recovered = runtime.store.recover(
            expected_assembly_id=runtime.engine.assembly.assembly_id
        )
        summary["cases"][case_dir.name] = {
            "patch_count": len(assembly.patches),
            "initial_assembly_version": initial.result.assembly.version_hash,
            "final_assembly_version": runtime.engine.assembly.version_hash,
            "actions": action_records,
            "trace_sha256": _sha256_file(trace_path),
            "manifest_chain_head": recovered.chain_head,
            "commit_seq": recovered.commit_seq,
        }

    (baseline / "baseline_hashes.json").write_bytes(
        canonical_json_bytes(summary) + b"\n"
    )
    return summary

def _sha256_file(path: Path) -> str:
    from .hashing import sha256_bytes

    return sha256_bytes(path.read_bytes())
