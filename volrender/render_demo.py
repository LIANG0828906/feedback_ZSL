"""Scripted, GUI-free rendering entry point for deterministic verification."""

from __future__ import annotations

import argparse
import json
import platform
import sys
from pathlib import Path
from typing import Any

import numpy as np

from .errors import VolRenderError
from .render import VolumeRenderer
from .transfer import TransferFunction
from .volume import VolumeData


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_VOLUME = ROOT / "samples" / "density_64.vol"
DEFAULT_CASES = ROOT / "cases" / "cases.json"
DEFAULT_OUTPUT = ROOT / "artifacts" / "demo"


def load_case_points(path: Path, name: str) -> list[dict[str, float]]:
    document = json.loads(path.read_text(encoding="utf-8"))
    for case in document.get("transfer_functions", []):
        if case["name"] == name:
            return case["points"]
    raise VolRenderError("E_INPUT_INVALID", f"transfer function case not found: {name}")


def write_ppm(path: Path, rgba8: np.ndarray) -> None:
    rgb = np.ascontiguousarray(rgba8[..., :3])
    height, width = rgb.shape[:2]
    header = f"P6\n{width} {height}\n255\n".encode("ascii")
    path.write_bytes(header + rgb.tobytes())


def write_error_ppm(path: Path, error_map: np.ndarray, threshold: float) -> None:
    ratio = np.clip(error_map / threshold, 0.0, 1.0)
    red = np.clip(2.0 * ratio, 0.0, 1.0)
    green = np.clip(2.0 - 2.0 * ratio, 0.0, 1.0)
    blue = np.clip(1.0 - ratio, 0.0, 1.0)
    rgb = np.stack((red, green, blue), axis=-1)
    rgba = np.zeros((*ratio.shape, 4), dtype=np.uint8)
    rgba[..., :3] = np.floor(rgb * 255.0 + 0.5).astype(np.uint8)
    rgba[..., 3] = 255
    write_ppm(path, rgba)


def platform_facts() -> dict[str, Any]:
    return {
        "python": platform.python_version(),
        "python_implementation": platform.python_implementation(),
        "numpy": np.__version__,
        "platform": platform.platform(),
        "machine": platform.machine(),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--volume", type=Path, default=DEFAULT_VOLUME)
    parser.add_argument("--mask", type=Path, default=None)
    parser.add_argument("--cases", type=Path, default=DEFAULT_CASES)
    parser.add_argument("--tf-case", default="medical_default")
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--image-size", type=int, default=96)
    parser.add_argument("--step", type=float, default=1.0)
    parser.add_argument("--mode", choices=("float", "fixed"), default="float")
    parser.add_argument("--threshold", type=float, default=1.0e-3)
    parser.add_argument("--skip-drift", action="store_true")
    args = parser.parse_args(argv)

    args.output_dir.mkdir(parents=True, exist_ok=True)
    volume = VolumeData.from_file(args.volume)
    mask_info = None
    if args.mask is not None:
        mask = VolumeData.from_file(args.mask)
        mask_info = {
            "path": str(args.mask),
            "sha256": mask.file_sha256,
            "shape": list(mask.shape_xyz),
            "labels": sorted(int(v) for v in np.unique(mask.raw)),
        }
    transfer = TransferFunction(load_case_points(args.cases, args.tf_case))
    renderer = VolumeRenderer(
        volume=volume,
        transfer=transfer,
        mode=args.mode,
        step=args.step,
        image_size=(args.image_size, args.image_size),
        error_threshold=args.threshold,
    )

    first = renderer.render()
    second = renderer.render()
    deterministic = (
        first.content_hash == second.content_hash
        and np.array_equal(first.rgba8, second.rgba8)
    )
    comparison = renderer.compare_precision()
    drift = None
    if not args.skip_drift:
        drift_report = renderer.audit_lighting_drift(raise_on_fail=False)
        drift = drift_report.as_dict()

    chosen = first
    image_path = args.output_dir / f"render_{args.mode}.ppm"
    error_path = args.output_dir / f"error_heatmap_{args.mode}.ppm"
    write_ppm(image_path, chosen.rgba8)
    write_error_ppm(error_path, chosen.error_map, args.threshold)
    snapshot_path = args.output_dir / f"render_{args.mode}.volsnap.json"
    renderer.save_snapshot(snapshot_path)

    summary = {
        "platform": platform_facts(),
        "inputs": {
            "volume": str(args.volume),
            "volume_sha256": volume.file_sha256,
            "mask": mask_info,
            "normalization_reference": volume.normalization_reference.as_dict(),
            "mapping_digest": volume.mapping_digest,
            "mapping_revision": volume.mapping_revision,
        },
        "view": {
            "matrix": renderer.view_matrix.tolist(),
            "step": renderer.step,
            "image_size": list(renderer.image_size),
            "fov_degrees": renderer.fov_degrees,
        },
        "transfer_function": transfer.as_list(),
        "render": chosen.as_dict(),
        "deterministic": deterministic,
        "precision_comparison": comparison.as_dict(),
        "lighting_drift": drift,
        "outputs": {
            "rgba_ppm": str(image_path),
            "error_ppm": str(error_path),
            "snapshot": str(snapshot_path),
        },
    }
    summary_path = args.output_dir / "summary.json"
    summary_path.write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0 if deterministic else 3


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except VolRenderError as exc:
        print(str(exc), file=sys.stderr)
        raise SystemExit(2) from exc
