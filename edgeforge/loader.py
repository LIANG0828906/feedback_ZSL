from __future__ import annotations

from dataclasses import dataclass
import json
from pathlib import Path
from typing import Any

import numpy as np

from .errors import DecodeError, MetaError
from .hashing import sha256_array
from .image_io import read_png, read_png_header


@dataclass(frozen=True)
class LoadedSample:
    sample_id: str
    image: np.ndarray
    truth_mask: np.ndarray
    meta: dict[str, Any]
    image_path: Path
    truth_path: Path
    meta_path: Path
    image_sha256: str
    truth_sha256: str

    @property
    def width(self) -> int:
        return int(self.image.shape[1])

    @property
    def height(self) -> int:
        return int(self.image.shape[0])

    @property
    def channels(self) -> int:
        return 1 if self.image.ndim == 2 else int(self.image.shape[2])

    @property
    def lines(self) -> list[dict[str, Any]]:
        return list(self.meta["truth"]["lines"])

    @property
    def circles(self) -> list[dict[str, Any]]:
        return list(self.meta["truth"]["circles"])


def load_sample(
    samples_root: str | Path,
    sample_id: str,
) -> LoadedSample:
    root = Path(samples_root)
    manifest_path = root / "manifest.json"
    manifest = _read_json(manifest_path, "manifest")
    if not isinstance(manifest, dict) or not isinstance(manifest.get("samples"), list):
        raise MetaError(
            "manifest.samples 缺失或格式错误",
            field="manifest.samples",
            details={"path": str(manifest_path)},
        )
    entry = None
    for candidate in manifest["samples"]:
        if isinstance(candidate, dict) and candidate.get("sample_id") == sample_id:
            entry = candidate
            break
    if entry is None:
        raise MetaError(
            "样本未在清单中声明",
            field="sample_id",
            details={"sample_id": sample_id, "manifest": str(manifest_path)},
        )

    sample_dir = root / sample_id
    meta_path = sample_dir / "meta.json"
    meta = _read_json(meta_path, "metadata")
    if not isinstance(meta, dict):
        raise MetaError(
            "样本元数据必须是对象",
            field="metadata",
            details={"path": str(meta_path)},
        )
    for field_name in (
        "sample_id",
        "width",
        "height",
        "channels",
        "image_file",
        "truth_mask_file",
        "image_sha256",
        "truth_mask_sha256",
        "truth",
    ):
        if field_name not in meta:
            raise MetaError(
                "样本元数据缺少字段",
                field=field_name,
                details={"path": str(meta_path)},
            )
    if meta["sample_id"] != sample_id:
        raise MetaError(
            "sample_id 与加载请求不一致",
            field="sample_id",
            details={"expected": sample_id, "actual": meta["sample_id"]},
        )

    image_path = sample_dir / str(meta["image_file"])
    truth_path = sample_dir / str(meta["truth_mask_file"])
    try:
        header = read_png_header(image_path)
    except FileNotFoundError as exc:
        raise DecodeError(
            "图像文件不存在",
            field="image_file",
            details={"path": str(image_path)},
        ) from exc

    width = _declared_int(meta, "width")
    height = _declared_int(meta, "height")
    channels = _declared_int(meta, "channels")
    if header["width"] != width:
        raise MetaError(
            "图像宽度与元数据不一致",
            field="width",
            details={"declared": width, "actual": header["width"]},
        )
    if header["height"] != height:
        raise MetaError(
            "图像高度与元数据不一致",
            field="height",
            details={"declared": height, "actual": header["height"]},
        )
    if header["channels"] != channels:
        raise MetaError(
            "图像通道数与元数据不一致",
            field="channels",
            details={"declared": channels, "actual": header["channels"]},
        )
    if channels not in {1, 3}:
        raise MetaError(
            "仅支持单通道或三通道图像",
            field="channels",
            details={"channels": channels},
        )

    image = read_png(image_path, channels)
    if image.shape != ((height, width) if channels == 1 else (height, width, channels)):
        raise MetaError(
            "解码后的图像形状与声明不一致",
            field="shape",
            details={
                "expected": [height, width] if channels == 1 else [height, width, channels],
                "actual": list(image.shape),
            },
        )
    image_hash = sha256_array(image)
    if image_hash != str(meta["image_sha256"]):
        raise DecodeError(
            "图像内容哈希不匹配，文件可能损坏",
            field="image_sha256",
            details={
                "expected": meta["image_sha256"],
                "actual": image_hash,
                "path": str(image_path),
            },
        )

    try:
        truth_mask = np.load(truth_path, allow_pickle=False)
    except (OSError, ValueError) as exc:
        raise DecodeError(
            "真值掩膜无法解码",
            field="truth_mask_file",
            details={"path": str(truth_path), "reason": str(exc)},
        ) from exc
    if truth_mask.dtype != np.uint8 or truth_mask.ndim != 2:
        raise MetaError(
            "真值掩膜必须是二维 uint8",
            field="truth_mask",
            details={"dtype": str(truth_mask.dtype), "shape": list(truth_mask.shape)},
        )
    if truth_mask.shape != (height, width):
        raise MetaError(
            "真值掩膜尺寸与图像不一致",
            field="truth_mask",
            details={
                "expected": [height, width],
                "actual": list(truth_mask.shape),
            },
        )
    truth_hash = sha256_array(truth_mask)
    if truth_hash != str(meta["truth_mask_sha256"]):
        raise DecodeError(
            "真值掩膜内容哈希不匹配",
            field="truth_mask_sha256",
            details={"expected": meta["truth_mask_sha256"], "actual": truth_hash},
        )
    if not np.isin(truth_mask, [0, 1]).all():
        raise MetaError(
            "真值掩膜只能包含 0 与 1",
            field="truth_mask",
            details={},
        )
    truth_mask_bool = truth_mask.astype(bool)

    truth_decl = meta["truth"]
    if not isinstance(truth_decl, dict):
        raise MetaError(
            "truth 字段必须是对象",
            field="truth",
            details={},
        )
    for field_name in ("lines", "circles", "edge_coverage"):
        if field_name not in truth_decl:
            raise MetaError(
                "真值清单缺少字段",
                field=f"truth.{field_name}",
                details={},
            )
    coverage = float(truth_decl["edge_coverage"])
    actual_coverage = float(truth_mask_bool.mean())
    if abs(coverage - actual_coverage) > 1e-12:
        raise MetaError(
            "真值边缘覆盖率与掩膜不一致",
            field="truth.edge_coverage",
            details={"declared": coverage, "actual": actual_coverage},
        )
    _validate_line_truth(truth_decl["lines"], width, height)
    _validate_circle_truth(truth_decl["circles"], width, height)

    if entry.get("width") != width or entry.get("height") != height:
        raise MetaError(
            "manifest 与样本元数据的尺寸不一致",
            field="width",
            details={
                "manifest": [entry.get("width"), entry.get("height")],
                "metadata": [width, height],
            },
        )
    if entry.get("channels") != channels:
        raise MetaError(
            "manifest 与样本元数据的通道数不一致",
            field="channels",
            details={"manifest": entry.get("channels"), "metadata": channels},
        )
    return LoadedSample(
        sample_id=sample_id,
        image=image,
        truth_mask=truth_mask_bool,
        meta=meta,
        image_path=image_path,
        truth_path=truth_path,
        meta_path=meta_path,
        image_sha256=image_hash,
        truth_sha256=truth_hash,
    )


def _read_json(path: Path, label: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise DecodeError(
            f"{label} JSON 无法读取或解析",
            details={"path": str(path), "reason": str(exc)},
        ) from exc


def _declared_int(meta: dict[str, Any], field_name: str) -> int:
    value = meta[field_name]
    if not isinstance(value, int) or value <= 0:
        raise MetaError(
            "声明字段必须是正整数",
            field=field_name,
            details={"value": value},
        )
    return value


def _validate_line_truth(lines: Any, width: int, height: int) -> None:
    if not isinstance(lines, list):
        raise MetaError("truth.lines 必须是数组", field="truth.lines", details={})
    for index, line in enumerate(lines):
        if not isinstance(line, dict):
            raise MetaError(
                "直线真值必须是对象",
                field=f"truth.lines[{index}]",
                details={},
            )
        for point_name in ("start", "end"):
            point = line.get(point_name)
            if (
                not isinstance(point, list)
                or len(point) != 2
                or not all(isinstance(value, int) for value in point)
            ):
                raise MetaError(
                    "直线端点必须是两个整数",
                    field=f"truth.lines[{index}].{point_name}",
                    details={},
                )
            x, y = point
            if not (0 <= x < width and 0 <= y < height):
                raise MetaError(
                    "直线端点越界",
                    field=f"truth.lines[{index}].{point_name}",
                    details={"x": x, "y": y, "width": width, "height": height},
                )


def _validate_circle_truth(circles: Any, width: int, height: int) -> None:
    if not isinstance(circles, list):
        raise MetaError("truth.circles 必须是数组", field="truth.circles", details={})
    for index, circle in enumerate(circles):
        if not isinstance(circle, dict):
            raise MetaError(
                "圆真值必须是对象",
                field=f"truth.circles[{index}]",
                details={},
            )
        center = circle.get("center")
        diameter = circle.get("diameter")
        if (
            not isinstance(center, list)
            or len(center) != 2
            or not all(isinstance(value, int) for value in center)
        ):
            raise MetaError(
                "圆心必须是两个整数",
                field=f"truth.circles[{index}].center",
                details={},
            )
        if not isinstance(diameter, int) or diameter <= 0:
            raise MetaError(
                "圆直径必须是正整数",
                field=f"truth.circles[{index}].diameter",
                details={},
            )
        x, y = center
        if not (0 <= x < width and 0 <= y < height):
            raise MetaError(
                "圆心越界",
                field=f"truth.circles[{index}].center",
                details={"x": x, "y": y},
            )
