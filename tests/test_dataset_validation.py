from __future__ import annotations

import json
import shutil
from pathlib import Path

import cv2
import numpy as np
import pytest

from panoworkbench.dataset import load_dataset
from panoworkbench.errors import ErrorCode, WorkbenchError


def _copy_dataset(source: Path, target: Path) -> Path:
    shutil.copytree(source, target)
    return target


def _manifest(root: Path) -> dict:
    return json.loads((root / "dataset.json").read_text(encoding="utf-8"))


def _write_manifest(root: Path, payload: dict) -> None:
    (root / "dataset.json").write_text(
        json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )


def test_valid_generated_dataset(sample_dataset) -> None:
    assert sample_dataset.image_count == 3
    assert sample_dataset.base_index == 0
    assert sample_dataset.theoretical_width > 0
    assert sample_dataset.theoretical_height > 0
    assert sample_dataset.content_hash
    assert not sample_dataset.errors
    for image in sample_dataset.images.values():
        assert image.width > 100
        assert image.height > 100
        assert image.channels == 3
        assert image.homography.shape == (3, 3)


def test_missing_manifest_is_e_decode(tmp_path: Path) -> None:
    with pytest.raises(WorkbenchError) as context:
        load_dataset(tmp_path)
    assert context.value.code is ErrorCode.DECODE


def test_corrupt_image_is_excluded_and_reported(sample_root: Path, tmp_path: Path) -> None:
    root = _copy_dataset(sample_root, tmp_path / "corrupt")
    (root / "images" / "view_001.png").write_bytes(b"not a png")
    dataset = load_dataset(root)
    assert 1 not in dataset.images
    assert any(
        error.code is ErrorCode.DECODE and error.filename == "images/view_001.png"
        for error in dataset.errors
    )
    assert dataset.image_count == 2


def test_corrupt_base_image_is_e_decode(sample_root: Path, tmp_path: Path) -> None:
    root = _copy_dataset(sample_root, tmp_path / "corrupt-base")
    (root / "images" / "view_000.png").write_bytes(b"not a png")
    with pytest.raises(WorkbenchError) as context:
        load_dataset(root)
    assert context.value.code is ErrorCode.DECODE
    assert context.value.filename == "images/view_000.png"


def test_dimension_mismatch_names_file_and_field(
    sample_root: Path, tmp_path: Path
) -> None:
    root = _copy_dataset(sample_root, tmp_path / "meta")
    manifest = _manifest(root)
    manifest["images"][1]["width"] += 9
    _write_manifest(root, manifest)
    dataset = load_dataset(root)
    assert 1 not in dataset.images
    error = next(error for error in dataset.errors if error.filename == "images/view_001.png")
    assert error.code is ErrorCode.META
    assert error.field == "width"


def test_image_count_mismatch_is_e_meta(sample_root: Path, tmp_path: Path) -> None:
    root = _copy_dataset(sample_root, tmp_path / "count")
    manifest = _manifest(root)
    manifest["image_count"] += 1
    _write_manifest(root, manifest)
    with pytest.raises(WorkbenchError) as context:
        load_dataset(root)
    assert context.value.code is ErrorCode.META
    assert context.value.field == "image_count"


def test_channel_mismatch_names_channels(sample_root: Path, tmp_path: Path) -> None:
    root = _copy_dataset(sample_root, tmp_path / "channels")
    path = root / "images" / "view_001.png"
    image = cv2.imread(str(path), cv2.IMREAD_COLOR)
    gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
    cv2.imwrite(str(path), gray)
    dataset = load_dataset(root)
    assert 1 not in dataset.images
    error = next(error for error in dataset.errors if error.filename == "images/view_001.png")
    assert error.code is ErrorCode.META
    assert error.field == "channels"


def test_declared_grayscale_image_is_read_in_both_modes(
    sample_root: Path, tmp_path: Path
) -> None:
    root = _copy_dataset(sample_root, tmp_path / "grayscale")
    path = root / "images" / "view_001.png"
    image = cv2.imread(str(path), cv2.IMREAD_COLOR)
    cv2.imwrite(str(path), cv2.cvtColor(image, cv2.COLOR_BGR2GRAY))
    manifest = _manifest(root)
    manifest["images"][1]["channels"] = 1
    _write_manifest(root, manifest)
    dataset = load_dataset(root)
    assert 1 in dataset.images
    assert dataset.read_image(1, color=True).shape[2] == 3
    assert dataset.read_image(1, color=False).ndim == 2


def test_singular_truth_homography_is_rejected(
    sample_root: Path, tmp_path: Path
) -> None:
    root = _copy_dataset(sample_root, tmp_path / "homography")
    manifest = _manifest(root)
    manifest["images"][1]["homography_to_base"] = [[0.0] * 3 for _ in range(3)]
    _write_manifest(root, manifest)
    dataset = load_dataset(root)
    assert 1 not in dataset.images
    error = next(error for error in dataset.errors if error.filename == "images/view_001.png")
    assert error.code is ErrorCode.META
    assert error.field == "homography_to_base"


def test_nonfinite_truth_homography_is_rejected(
    sample_root: Path, tmp_path: Path
) -> None:
    root = _copy_dataset(sample_root, tmp_path / "nonfinite")
    manifest = _manifest(root)
    matrix = np.eye(3).tolist()
    matrix[0][2] = float("nan")
    manifest["images"][2]["homography_to_base"] = matrix
    _write_manifest(root, manifest)
    dataset = load_dataset(root)
    assert 2 not in dataset.images
    error = next(error for error in dataset.errors if error.filename == "images/view_002.png")
    assert error.field == "homography_to_base"
