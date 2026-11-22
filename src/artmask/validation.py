"""Loading and strict metadata validation for generated samples."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np

from .errors import BusinessError
from .models import Sample


def _decode_error(message: str, path: Path, **details: Any) -> BusinessError:
    return BusinessError(
        "E_DECODE",
        message,
        details={"file": str(path.resolve()), **details},
    )


def _meta_error(message: str, field: str, **details: Any) -> BusinessError:
    return BusinessError(
        "E_META",
        message,
        field=field,
        details=details,
    )


def _read_manifest(path: Path) -> dict[str, Any]:
    if not path.is_file():
        raise _decode_error("清单文件缺失", path)
    try:
        with path.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise _decode_error("清单文件无法解析", path, error=str(exc)) from exc
    if not isinstance(data, dict):
        raise _decode_error("清单根节点必须是对象", path)
    return data


def _load_array(path: Path, label: str) -> np.ndarray:
    if not path.is_file():
        raise _decode_error(f"{label}文件缺失", path)
    try:
        array = np.load(path, allow_pickle=False)
    except (OSError, ValueError, EOFError) as exc:
        raise _decode_error(f"{label}文件损坏或无法解码", path, error=str(exc)) from exc
    if not isinstance(array, np.ndarray):
        raise _decode_error(f"{label}不是有效的数组", path)
    return array


def array_hash(array: np.ndarray) -> str:
    return hashlib.sha256(
        array.tobytes(order="C")
        + str(array.shape).encode()
        + str(array.dtype).encode()
    ).hexdigest()


def load_sample(directory: str | Path) -> Sample:
    directory = Path(directory).resolve()
    manifest_path = directory / "manifest.json" if directory.is_dir() else directory
    if not manifest_path.is_file() and directory.is_dir():
        manifest_path = directory / "manifest.json"
    manifest = _read_manifest(manifest_path)

    image_path = manifest_path.parent / str(manifest.get("image_file", "image.npy"))
    mask_path = manifest_path.parent / str(
        manifest.get("truth_mask_file", "truth_mask.npy")
    )
    image = _load_array(image_path, "图像")
    truth = _load_array(mask_path, "真值掩码")

    if image.ndim != 3:
        raise _meta_error(
            "图像必须是 H x W x C 数组",
            "channels",
            expected=3,
            actual_shape=list(image.shape),
            file=str(image_path.resolve()),
        )
    if image.dtype != np.uint8:
        raise _meta_error(
            "图像数据类型必须为 uint8",
            "dtype",
            expected="uint8",
            actual=str(image.dtype),
            file=str(image_path.resolve()),
        )
    height, width, channels = image.shape
    for field_name, expected, actual in (
        ("width", manifest.get("width"), width),
        ("height", manifest.get("height"), height),
        ("channels", manifest.get("channels"), channels),
    ):
        if expected != actual:
            raise _meta_error(
                f"清单字段 {field_name} 与图像不一致",
                field_name,
                expected=expected,
                actual=actual,
                file=str(image_path.resolve()),
            )
    if truth.ndim != 2 or truth.shape != (height, width):
        raise _meta_error(
            "真值掩码尺寸必须与图像一致",
            "truth_mask.shape",
            expected=[height, width],
            actual=list(truth.shape),
            file=str(mask_path.resolve()),
        )
    if truth.dtype.kind not in "ui":
        raise _meta_error(
            "真值掩码必须是整数类型",
            "truth_mask.dtype",
            actual=str(truth.dtype),
        )

    expected_image_hash = manifest.get("image_hash")
    if expected_image_hash and expected_image_hash != array_hash(image):
        raise _decode_error(
            "图像内容哈希与清单不一致",
            image_path,
            expected=expected_image_hash,
            actual=array_hash(image),
        )
    expected_mask_hash = manifest.get("truth_mask_hash")
    if expected_mask_hash and expected_mask_hash != array_hash(truth):
        raise _decode_error(
            "真值掩码内容哈希与清单不一致",
            mask_path,
            expected=expected_mask_hash,
            actual=array_hash(truth),
        )

    unique_labels = [int(value) for value in np.unique(truth) if int(value) != 0]
    declared_count = manifest.get("region_count")
    if declared_count != len(unique_labels):
        raise _meta_error(
            "清单区域数量与真值掩码标签数量不一致",
            "region_count",
            expected=declared_count,
            actual=len(unique_labels),
        )
    declared_regions = manifest.get("regions")
    if not isinstance(declared_regions, list) or len(declared_regions) != declared_count:
        raise _meta_error(
            "清单区域列表缺失或数量错误",
            "regions",
            expected=declared_count,
            actual=len(declared_regions) if isinstance(declared_regions, list) else None,
        )
    declared_ids = sorted(int(item.get("id", -1)) for item in declared_regions)
    if declared_ids != unique_labels:
        raise _meta_error(
            "清单区域编号与真值掩码标签不一致",
            "regions[].id",
            expected=unique_labels,
            actual=declared_ids,
        )
    for region in declared_regions:
        region_id = int(region["id"])
        actual_area = int(np.count_nonzero(truth == region_id))
        if int(region.get("area", -1)) != actual_area:
            raise _meta_error(
                "清单区域面积与真值掩码不一致",
                f"regions[{region_id}].area",
                expected=region.get("area"),
                actual=actual_area,
            )

    return Sample(
        sample_id=str(manifest.get("sample_id", manifest_path.parent.name)),
        scene=str(manifest.get("scene", "unknown")),
        directory=manifest_path.parent,
        manifest=manifest,
        image=image,
        truth_mask=truth,
    )


def load_suite_index(root: str | Path) -> dict[str, Any]:
    path = Path(root).resolve() / "manifest.json"
    data = _read_manifest(path)
    samples = data.get("samples")
    if not isinstance(samples, list):
        raise _meta_error("样本索引缺少 samples 字段", "samples")
    return data


def load_suite(root: str | Path) -> list[Sample]:
    root = Path(root).resolve()
    index = load_suite_index(root)
    results = []
    for entry in index["samples"]:
        directory = Path(
            entry.get("directory", root / str(entry.get("sample_id", "")))
        )
        results.append(load_sample(directory))
    return results
