"""Global registration graph, per-pair report, and output comparison."""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import cv2
import numpy as np

from .config import ALGORITHM_VERSION, PipelineConfig
from .dataset import Dataset
from .errors import ErrorCode, WorkbenchError
from .geometry import CanvasGeometry, invert_homography, normalize_homography
from .state import PairRecord, PairRegistry, PairState, PanoramaState
from .strategy import StrategyDecision


@dataclass(frozen=True)
class GlobalRegistration:
    homographies_to_base: dict[int, np.ndarray]
    pair_id_by_image: dict[int, str]
    hops_by_image: dict[int, int]
    missing_images: tuple[int, ...]


def build_global_registration(
    registry: PairRegistry,
    base_index: int,
    image_indices: list[int],
) -> GlobalRegistration:
    """Compose committed pair transforms along shortest edge paths to base."""

    adjacency: dict[int, list[tuple[int, np.ndarray, str]]] = {
        index: [] for index in image_indices
    }
    for record in registry.ordered():
        if record.state is not PairState.COMMITTED or record.result is None:
            continue
        result = record.result
        h = normalize_homography(result.homography)
        try:
            inverse = invert_homography(h)
        except WorkbenchError:
            continue
        if record.image_a in adjacency:
            adjacency[record.image_a].append((record.image_b, inverse, record.pair_id))
        if record.image_b in adjacency:
            adjacency[record.image_b].append((record.image_a, h, record.pair_id))

    homographies: dict[int, np.ndarray] = {base_index: np.eye(3, dtype=np.float64)}
    pair_by_image: dict[int, str] = {base_index: "BASE"}
    hops: dict[int, int] = {base_index: 0}
    queue: deque[int] = deque([base_index])
    while queue:
        current = queue.popleft()
        for neighbor, transform, pair_id in sorted(
            adjacency.get(current, []), key=lambda item: (item[0], item[2])
        ):
            if neighbor in homographies:
                continue
            homographies[neighbor] = normalize_homography(
                homographies[current] @ transform
            )
            pair_by_image[neighbor] = pair_id
            hops[neighbor] = hops[current] + 1
            queue.append(neighbor)

    missing = tuple(sorted(index for index in image_indices if index not in homographies))
    return GlobalRegistration(
        homographies_to_base=homographies,
        pair_id_by_image=pair_by_image,
        hops_by_image=hops,
        missing_images=missing,
    )


def truth_pair_homography(dataset: Dataset, image_a: int, image_b: int) -> np.ndarray:
    h_a = dataset.images[image_a].homography
    h_b = dataset.images[image_b].homography
    return normalize_homography(invert_homography(h_b) @ h_a)


def make_report(
    *,
    dataset: Dataset,
    config: PipelineConfig,
    decision: StrategyDecision,
    parameter_version: str,
    registry: PairRegistry,
    global_registration: GlobalRegistration,
    geometry: CanvasGeometry | None,
    panorama_state: PanoramaState,
    status: str,
    feature_counts: dict[int, int],
    panorama_info: dict[str, Any] | None,
    warnings: list[str] | None = None,
) -> dict[str, Any]:
    image_entries: list[dict[str, Any]] = []
    for index in sorted(dataset.images):
        meta = dataset.images[index]
        h_to_base = global_registration.homographies_to_base.get(index)
        h_to_canvas = (
            normalize_homography(geometry.translation @ h_to_base)
            if geometry is not None and h_to_base is not None
            else None
        )
        truth_ok = None
        if h_to_base is not None:
            truth = meta.homography
            corners = np.array(
                [
                    [0.0, 0.0],
                    [float(meta.width), 0.0],
                    [float(meta.width), float(meta.height)],
                    [0.0, float(meta.height)],
                ],
                dtype=np.float64,
            )
            estimated_points = cv2.perspectiveTransform(
                corners.reshape(1, -1, 2), h_to_base
            ).reshape(-1, 2)
            truth_points = cv2.perspectiveTransform(
                corners.reshape(1, -1, 2), truth
            ).reshape(-1, 2)
            error = float(np.max(np.linalg.norm(estimated_points - truth_points, axis=1)))
            truth_ok = error <= config.homography.truth_corner_tolerance
        else:
            error = None
        image_entries.append(
            {
                "id": index,
                "filename": meta.filename,
                "width": meta.width,
                "height": meta.height,
                "channels": meta.channels,
                "sha256": meta.sha256,
                "feature_count": feature_counts.get(index, 0),
                "registered": h_to_base is not None,
                "homography_to_base": (
                    [float(v) for v in h_to_base.reshape(-1)]
                    if h_to_base is not None
                    else None
                ),
                "homography_to_canvas": (
                    [float(v) for v in h_to_canvas.reshape(-1)]
                    if h_to_canvas is not None
                    else None
                ),
                "registration_pair_id": global_registration.pair_id_by_image.get(index),
                "registration_hops": global_registration.hops_by_image.get(index),
                "truth_corner_error_px": error,
                "truth_ok": truth_ok,
            }
        )

    pairs = [record.to_dict() for record in registry.ordered()]
    committed = sum(
        record.state is PairState.COMMITTED for record in registry.records.values()
    )
    truth_errors = [
        record.result.truth_corner_error
        for record in registry.ordered()
        if record.result is not None and record.result.truth_corner_error is not None
    ]
    image_truth_errors = [
        entry["truth_corner_error_px"]
        for entry in image_entries
        if entry["truth_corner_error_px"] is not None
    ]
    report: dict[str, Any] = {
        "schema_version": 1,
        "algorithm_version": ALGORITHM_VERSION,
        "parameter_version": parameter_version,
        "status": status,
        "dataset": {
            "name": dataset.name,
            "seed": dataset.seed,
            "content_hash": dataset.content_hash,
            "manifest_sha256": dataset.manifest_sha256,
            "base_image": dataset.base_index,
            "image_count": dataset.image_count,
            "validation_errors": [error.to_dict() for error in dataset.errors],
            "theoretical_canvas": {
                "width": dataset.theoretical_width,
                "height": dataset.theoretical_height,
                "origin_x": dataset.theoretical_origin[0],
                "origin_y": dataset.theoretical_origin[1],
            },
        },
        "parameters": config.to_dict(),
        "strategy": decision.to_dict(),
        "progress": {
            "committed_pairs": committed,
            "total_pairs": registry.total_count,
            "panorama_state": panorama_state.value,
            "panorama_complete": panorama_state is PanoramaState.COMMITTED,
        },
        "images": image_entries,
        "pairs": pairs,
        "registration": {
            "missing_images": list(global_registration.missing_images),
            "max_pair_truth_corner_error_px": max(truth_errors) if truth_errors else None,
            "max_image_truth_corner_error_px": (
                max(image_truth_errors) if image_truth_errors else None
            ),
            "all_pair_truth_ok": all(
                record.result.truth_ok
                for record in registry.ordered()
                if record.result is not None and record.result.truth_ok is not None
            )
            if truth_errors
            else None,
            "all_images_truth_ok": all(
                entry["truth_ok"]
                for entry in image_entries
                if entry["truth_ok"] is not None
            )
            if image_truth_errors
            else None,
        },
        "panorama": panorama_info,
        "warnings": warnings or [],
    }
    return report


def canonical_report(report: dict[str, Any]) -> dict[str, Any]:
    """Strip fields that are allowed to differ between equivalent run paths."""

    canonical = dict(report)
    if "panorama" in canonical and isinstance(canonical["panorama"], dict):
        panorama = dict(canonical["panorama"])
        panorama.pop("output_path", None)
        panorama.pop("owner_path", None)
        panorama.pop("seams_path", None)
        panorama.pop("error_heatmap_path", None)
        canonical["panorama"] = panorama
    return canonical


@dataclass(frozen=True)
class PanoramaComparison:
    identical: bool
    first_diff_xy: tuple[int, int] | None
    first_diff_bgr_a: tuple[int, int, int] | None
    first_diff_bgr_b: tuple[int, int, int] | None
    owner_image: int | None
    pair_id: str | None
    report_identical: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "identical": self.identical,
            "first_diff_xy": list(self.first_diff_xy) if self.first_diff_xy else None,
            "first_diff_bgr_a": (
                list(self.first_diff_bgr_a) if self.first_diff_bgr_a else None
            ),
            "first_diff_bgr_b": (
                list(self.first_diff_bgr_b) if self.first_diff_bgr_b else None
            ),
            "owner_image": self.owner_image,
            "pair_id": self.pair_id,
            "report_identical": self.report_identical,
        }


def _load_owner(path: Path) -> np.ndarray:
    with np.load(path) as data:
        return np.asarray(data["owner"], dtype=np.int16)


def compare_panorama_outputs(
    output_a: str | Path,
    output_b: str | Path,
    *,
    pair_report: dict[str, Any] | None = None,
) -> PanoramaComparison:
    root_a = Path(output_a)
    root_b = Path(output_b)
    image_a = cv2.imread(str(root_a / "panorama.png"), cv2.IMREAD_COLOR)
    image_b = cv2.imread(str(root_b / "panorama.png"), cv2.IMREAD_COLOR)
    if image_a is None or image_b is None:
        raise WorkbenchError(
            ErrorCode.DECODE,
            "one or both panorama outputs are missing or unreadable",
            filename=str(root_a if image_a is None else root_b),
        )
    report_a_path = root_a / "report.json"
    report_b_path = root_b / "report.json"
    report_identical = False
    report_a: dict[str, Any] | None = None
    if report_a_path.is_file() and report_b_path.is_file():
        import json

        try:
            report_a = json.loads(report_a_path.read_text(encoding="utf-8"))
            report_b = json.loads(report_b_path.read_text(encoding="utf-8"))
            report_identical = canonical_report(report_a) == canonical_report(report_b)
        except (OSError, ValueError):
            report_identical = False
    if image_a.shape != image_b.shape:
        return PanoramaComparison(
            False, (0, 0), None, None, None, None, report_identical
        )
    difference = np.any(image_a != image_b, axis=2)
    if not np.any(difference):
        return PanoramaComparison(True, None, None, None, None, None, report_identical)
    y, x = (int(value) for value in np.argwhere(difference)[0])
    owner_image: int | None = None
    pair_id: str | None = None
    try:
        owner = _load_owner(root_a / "owner.npz")
        if 0 <= y < owner.shape[0] and 0 <= x < owner.shape[1]:
            owner_image = int(owner[y, x])
            if owner_image >= 0 and report_a is not None:
                for image_entry in report_a.get("images", []):
                    if image_entry.get("id") == owner_image:
                        pair_id = image_entry.get("registration_pair_id")
                        break
    except (OSError, ValueError, KeyError):
        pass
    return PanoramaComparison(
        False,
        (x, y),
        tuple(int(v) for v in image_a[y, x]),
        tuple(int(v) for v in image_b[y, x]),
        owner_image,
        pair_id,
        report_identical,
    )
