from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path
from typing import Any

import numpy as np

from .cancellation import CancellationToken
from .errors import AcousticError
from .hashing import canonical_json, sha256_array
from .model import Scene, apply_factor, load_materials
from .service import AcousticEngine, RebuildReport


class _CancelBeforeToken(CancellationToken):
    def __init__(self, node: str):
        super().__init__()
        self.node = node
        self.triggered = False

    def checkpoint(self, location: str) -> None:
        if not self.triggered and location.endswith(f":{self.node}"):
            self.triggered = True
            self.cancel()
        super().checkpoint(location)


def _report_trace(report: RebuildReport) -> dict[str, Any]:
    payload = report.to_dict()
    for point in payload["points"]:
        if point.get("coefficient_path"):
            point["coefficient_path"] = Path(point["coefficient_path"]).name
    return payload


def _load_changes(path: Path) -> list[dict[str, Any]]:
    payload = json.loads(path.read_text(encoding="utf-8"))
    if isinstance(payload, dict):
        payload = payload.get("changes", payload.get("sequence"))
    if not isinstance(payload, list):
        raise ValueError("modification sequence must be a JSON array")
    return payload


def _fresh_dir(path: Path, keep: bool) -> None:
    if path.exists() and not keep:
        shutil.rmtree(path)
    path.mkdir(parents=True, exist_ok=True)


def _point_coefficients(engine: AcousticEngine, scene: Scene, point_id: str) -> np.ndarray:
    result, coefficients = engine.validate_field(scene, point_id)
    return coefficients


def run_diagnostics(
    sample_dir: Path,
    changes_path: Path,
    trace_path: Path,
    *,
    output_dir: Path = Path("artifacts/diagnostics"),
    cache_dir: Path | None = None,
    full_cache_dir: Path | None = None,
    keep_cache: bool = False,
    cancel_before_node: str | None = None,
) -> dict[str, Any]:
    sample_dir = sample_dir.resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    trace_path.parent.mkdir(parents=True, exist_ok=True)
    cache_dir = cache_dir or (output_dir / "cache-incremental")
    full_cache_dir = full_cache_dir or (output_dir / "cache-full")
    _fresh_dir(cache_dir, keep_cache)
    _fresh_dir(full_cache_dir, keep_cache)

    scene = Scene.load(sample_dir / "scene.json")
    materials = load_materials(sample_dir / scene.materials_file)
    changes = _load_changes(changes_path)
    engine = AcousticEngine(sample_dir, cache_dir=cache_dir, scene=scene, materials=materials)
    records: list[dict[str, Any]] = []
    current_scene = scene
    token = _CancelBeforeToken(cancel_before_node) if cancel_before_node else None

    def append(record: dict[str, Any]) -> None:
        records.append(record)
        with trace_path.open("a", encoding="utf-8") as handle:
            handle.write(canonical_json(record) + "\n")

    trace_path.write_text("", encoding="utf-8")
    try:
        baseline = engine.rebuild(current_scene, mode="baseline", token=token)
        append(
            {
                "sequence": "baseline",
                "input_sha256": {
                    "scene": sha256_array(
                        np.frombuffer(
                            (sample_dir / "scene.json").read_bytes(),
                            dtype=np.uint8,
                        )
                    ),
                    "materials": sha256_array(
                        np.frombuffer(
                            (sample_dir / scene.materials_file).read_bytes(),
                            dtype=np.uint8,
                        )
                    ),
                    "impulse_responses": {
                        point_id: sha256_array(
                            np.frombuffer(
                                (sample_dir / path).read_bytes(),
                                dtype=np.uint8,
                            )
                        )
                        for point_id, path in sorted(scene.ir_files.items())
                    },
                    "test_signal": sha256_array(
                        np.frombuffer(
                            scene.resolved_test_signal(sample_dir).read_bytes(),
                            dtype=np.uint8,
                        )
                    ),
                },
                "report": _report_trace(baseline),
            }
        )

        for index, change in enumerate(changes, start=1):
            previous_scene = current_scene
            current_scene, factor, scope = apply_factor(
                current_scene,
                materials,
                change,
            )
            report = engine.rebuild(
                current_scene,
                changes=[change],
                previous_scene=previous_scene,
                mode="incremental",
                token=token,
            )
            append(
                {
                    "sequence": index,
                    "change": change,
                    "factor": factor,
                    "scope": scope,
                    "report": _report_trace(report),
                }
            )

        render_records = []
        for listener in current_scene.listeners:
            render = engine.render_point(
                current_scene,
                listener.id,
                output_path=output_dir / "renders" / f"incremental_{listener.id}.wav",
            )
            render_records.append(
                {
                    "point_id": listener.id,
                    "render_cache_key": render["cache_key"],
                    "field_version": render["field_version"],
                    "wav_sha256": render["wav_sha256"],
                    "frames": render["frames"],
                    "wav": Path(render["render_path"]).name,
                }
            )
        append({"sequence": "incremental_render", "renders": render_records})

        full_engine = AcousticEngine(
            sample_dir,
            cache_dir=full_cache_dir,
            scene=current_scene,
            materials=materials,
        )
        full_report = full_engine.rebuild(current_scene, mode="full")
        comparisons = []
        for listener in current_scene.listeners:
            incremental_coeffs = _point_coefficients(
                engine, current_scene, listener.id
            )
            full_coeffs = _point_coefficients(
                full_engine, current_scene, listener.id
            )
            incremental_hash = sha256_array(incremental_coeffs)
            full_hash = sha256_array(full_coeffs)
            difference = float(
                np.max(np.abs(incremental_coeffs - full_coeffs))
            ) if incremental_coeffs.shape == full_coeffs.shape else float("inf")
            comparisons.append(
                {
                    "point_id": listener.id,
                    "incremental_hash": incremental_hash,
                    "full_hash": full_hash,
                    "byte_identical": incremental_hash == full_hash,
                    "max_abs_difference": difference,
                    "within_tolerance": bool(difference <= 1e-12),
                }
            )
        append(
            {
                "sequence": "full_recompute_comparison",
                "full_report": _report_trace(full_report),
                "comparisons": comparisons,
                "all_within_tolerance": all(
                    item["within_tolerance"] for item in comparisons
                ),
                "all_byte_identical": all(
                    item["byte_identical"] for item in comparisons
                ),
            }
        )
        summary = {
            "trace": str(trace_path),
            "records": len(records),
            "comparisons": comparisons,
            "all_within_tolerance": all(
                item["within_tolerance"] for item in comparisons
            ),
        }
        _write_final_scene(output_dir / "final_scene.json", current_scene)
        return summary
    except AcousticError as exc:
        append(
            {
                "sequence": "error",
                "error": exc.to_dict(),
                "status": "aborted",
            }
        )
        raise


def _write_final_scene(path: Path, scene: Scene) -> None:
    path.write_text(canonical_json(scene.to_dict()), encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run deterministic incremental acoustic-field diagnostics."
    )
    parser.add_argument(
        "--samples",
        "--samples-dir",
        help="venue sample directory",
    )
    parser.add_argument(
        "--changes",
        "--changes-file",
        help="JSON array containing influence-factor modifications",
    )
    parser.add_argument(
        "--trace",
        "--output-trace",
        help="output JSONL trace path",
    )
    parser.add_argument(
        "--output-dir",
        default="artifacts/diagnostics",
        help="diagnostic cache and render output directory",
    )
    parser.add_argument("--cache-dir", help="incremental cache directory")
    parser.add_argument("--full-cache-dir", help="full-recompute cache directory")
    parser.add_argument(
        "--keep-cache",
        action="store_true",
        help="reuse existing diagnostic caches (default: deterministic fresh run)",
    )
    parser.add_argument(
        "--cancel-before-node",
        choices=[
            "direct_align",
            "early_reflections",
            "band_decay",
            "late_reverb",
            "multichannel_stitch",
            "field_coefficients",
        ],
        help="exercise cancellation at the first matching checkpoint",
    )
    parser.add_argument(
        "paths",
        nargs="*",
        help="optional positional SAMPLES CHANGES TRACE",
    )
    args = parser.parse_args(argv)
    if args.paths:
        if len(args.paths) != 3:
            parser.error("positional form requires SAMPLES CHANGES TRACE")
        args.samples = args.samples or args.paths[0]
        args.changes = args.changes or args.paths[1]
        args.trace = args.trace or args.paths[2]
    if not args.samples or not args.changes or not args.trace:
        parser.error("--samples, --changes and --trace are required")
    try:
        summary = run_diagnostics(
            Path(args.samples),
            Path(args.changes),
            Path(args.trace),
            output_dir=Path(args.output_dir),
            cache_dir=Path(args.cache_dir) if args.cache_dir else None,
            full_cache_dir=Path(args.full_cache_dir) if args.full_cache_dir else None,
            keep_cache=args.keep_cache,
            cancel_before_node=args.cancel_before_node,
        )
    except AcousticError as exc:
        print(canonical_json(exc.to_dict()), file=sys.stderr)
        return 2
    print(canonical_json(summary))
    return 0 if summary["all_within_tolerance"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

