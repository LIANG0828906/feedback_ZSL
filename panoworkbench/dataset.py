"""Dataset loading, structural validation, and content hashing."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import cv2
import numpy as np

from .errors import ErrorCode, WorkbenchError
from .geometry import normalize_homography


@dataclass(frozen=True)
class ImageMeta:
    index: int
    filename: str
    abs_path: Path
    width: int
    height: int
    channels: int
    homography_to_base: tuple[float, ...]
    sha256: str
    overlap_with_base: tuple[dict[str, list[float]], ...] = ()
    synthetic_exposure: dict[str, float] = field(default_factory=dict)

    @property
    def homography(self) -> np.ndarray:
        result = np.asarray(self.homography_to_base, dtype=np.float64).reshape(3, 3)
        result.setflags(write=False)
        return result

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.index,
            "filename": self.filename,
            "width": self.width,
            "height": self.height,
            "channels": self.channels,
            "sha256": self.sha256,
            "homography_to_base": list(self.homography_to_base),
        }


@dataclass
class Dataset:
    root: Path
    manifest_path: Path
    manifest: dict[str, Any]
    name: str
    seed: int
    base_index: int
    theoretical_width: int
    theoretical_height: int
    theoretical_origin: tuple[int, int]
    images: dict[int, ImageMeta]
    errors: list[WorkbenchError] = field(default_factory=list)
    manifest_sha256: str = ""
    content_hash: str = ""

    @property
    def image_count(self) -> int:
        return len(self.images)

    @property
    def total_pixels(self) -> int:
        return sum(image.width * image.height for image in self.images.values())

    @property
    def average_megapixels(self) -> float:
        if not self.images:
            return 0.0
        return self.total_pixels / len(self.images) / 1_000_000.0

    def read_image(self, index: int, *, color: bool = True) -> np.ndarray:
        meta = self.images[index]
        data = np.fromfile(meta.abs_path, dtype=np.uint8)
        image = cv2.imdecode(data, cv2.IMREAD_UNCHANGED)
        if image is None:
            raise WorkbenchError(
                ErrorCode.DECODE,
                "image could not be decoded",
                filename=meta.filename,
            )
        if (int(image.shape[1]), int(image.shape[0])) != (meta.width, meta.height):
            raise WorkbenchError(
                ErrorCode.META,
                "image dimensions changed after initial validation",
                filename=meta.filename,
                field="width/height",
            )
        actual_channels = 1 if image.ndim == 2 else int(image.shape[2])
        if actual_channels != meta.channels:
            raise WorkbenchError(
                ErrorCode.META,
                "image channels changed after initial validation",
                filename=meta.filename,
                field="channels",
            )
        if color:
            if actual_channels == 1:
                image = cv2.cvtColor(image, cv2.COLOR_GRAY2BGR)
            elif actual_channels == 4:
                image = cv2.cvtColor(image, cv2.COLOR_BGRA2BGR)
        else:
            if actual_channels == 3:
                image = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
            elif actual_channels == 4:
                image = cv2.cvtColor(image, cv2.COLOR_BGRA2GRAY)
        return image

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "seed": self.seed,
            "root": str(self.root),
            "base_index": self.base_index,
            "image_count": self.image_count,
            "theoretical_canvas": {
                "width": self.theoretical_width,
                "height": self.theoretical_height,
                "origin_x": self.theoretical_origin[0],
                "origin_y": self.theoretical_origin[1],
            },
            "images": [self.images[key].to_dict() for key in sorted(self.images)],
            "errors": [error.to_dict() for error in self.errors],
            "content_hash": self.content_hash,
        }


def _read_manifest(path: Path) -> dict[str, Any]:
    if not path.is_file():
        raise WorkbenchError(
            ErrorCode.DECODE,
            "dataset manifest is missing",
            filename=str(path),
            field="dataset.json",
        )
    try:
        raw = path.read_bytes()
        payload = json.loads(raw.decode("utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise WorkbenchError(
            ErrorCode.DECODE,
            f"dataset manifest cannot be decoded: {exc}",
            filename=str(path),
            field="dataset.json",
        ) from exc
    if not isinstance(payload, dict):
        raise WorkbenchError(
            ErrorCode.DECODE,
            "dataset manifest root must be an object",
            filename=str(path),
            field="dataset.json",
        )
    return payload


def _safe_image_path(root: Path, filename: Any, image_id: Any) -> tuple[Path, str]:
    if not isinstance(filename, str) or not filename:
        raise WorkbenchError(
            ErrorCode.META,
            "image filename must be a non-empty relative path",
            filename=str(image_id),
            field="filename",
        )
    candidate = (root / filename).resolve()
    try:
        candidate.relative_to(root.resolve())
    except ValueError as exc:
        raise WorkbenchError(
            ErrorCode.META,
            "image filename escapes dataset root",
            filename=filename,
            field="filename",
        ) from exc
    return candidate, filename


def _decode_image(path: Path, filename: str) -> tuple[np.ndarray, bytes]:
    if not path.is_file():
        raise WorkbenchError(
            ErrorCode.DECODE, "image file is missing", filename=filename
        )
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise WorkbenchError(
            ErrorCode.DECODE, f"image file cannot be read: {exc}", filename=filename
        ) from exc
    decoded = cv2.imdecode(np.frombuffer(raw, dtype=np.uint8), cv2.IMREAD_UNCHANGED)
    if decoded is None:
        raise WorkbenchError(
            ErrorCode.DECODE, "image could not be decoded", filename=filename
        )
    if decoded.ndim == 2:
        channels = 1
    elif decoded.ndim == 3:
        channels = int(decoded.shape[2])
    else:
        raise WorkbenchError(
            ErrorCode.DECODE, "unsupported image dimensionality", filename=filename
        )
    return decoded, raw


def load_dataset(dataset_dir: str | Path, *, strict: bool = False) -> Dataset:
    """Load and validate a generated dataset.

    Missing/broken manifests and an unusable base image are fatal. Individual
    image failures are collected in ``Dataset.errors`` and excluded from
    ``Dataset.images`` so they can never enter registration.
    """

    root = Path(dataset_dir).resolve()
    manifest_path = root / "dataset.json"
    manifest = _read_manifest(manifest_path)
    raw_manifest = manifest_path.read_bytes()
    manifest_sha = hashlib.sha256(raw_manifest).hexdigest()
    errors: list[WorkbenchError] = []
    failed_by_id: dict[int, WorkbenchError] = {}
    valid_images: dict[int, ImageMeta] = {}
    file_hashes: list[tuple[str, str]] = []

    try:
        base_index = int(manifest.get("base_image", 0))
    except (TypeError, ValueError) as exc:
        raise WorkbenchError(
            ErrorCode.META,
            "base_image must be an integer",
            filename="dataset.json",
            field="base_image",
        ) from exc

    canvas = manifest.get("canvas")
    if not isinstance(canvas, dict):
        raise WorkbenchError(
            ErrorCode.META,
            "canvas truth object is missing",
            filename="dataset.json",
            field="canvas",
        )
    try:
        theoretical_width = int(canvas["width"])
        theoretical_height = int(canvas["height"])
        origin_x = int(canvas.get("origin_x", 0))
        origin_y = int(canvas.get("origin_y", 0))
    except (KeyError, TypeError, ValueError) as exc:
        raise WorkbenchError(
            ErrorCode.META,
            "canvas width/height must be positive integers",
            filename="dataset.json",
            field="canvas.width/canvas.height",
        ) from exc
    if theoretical_width <= 0 or theoretical_height <= 0:
        raise WorkbenchError(
            ErrorCode.META,
            "canvas dimensions must be positive",
            filename="dataset.json",
            field="canvas.width/canvas.height",
        )

    entries = manifest.get("images")
    if not isinstance(entries, list) or not entries:
        raise WorkbenchError(
            ErrorCode.META,
            "images must be a non-empty array",
            filename="dataset.json",
            field="images",
        )
    if "image_count" in manifest:
        try:
            declared_count = int(manifest["image_count"])
        except (TypeError, ValueError) as exc:
            raise WorkbenchError(
                ErrorCode.META,
                "image_count must be an integer",
                filename="dataset.json",
                field="image_count",
            ) from exc
        if declared_count != len(entries):
            raise WorkbenchError(
                ErrorCode.META,
                f"image_count={declared_count} does not match images length={len(entries)}",
                filename="dataset.json",
                field="image_count",
            )

    seen: set[int] = set()
    for position, entry in enumerate(entries):
        image_id: Any = position
        filename_for_error = f"images[{position}]"
        try:
            if not isinstance(entry, dict):
                raise WorkbenchError(
                    ErrorCode.META,
                    "image entry must be an object",
                    filename=filename_for_error,
                    field="images",
                )
            image_id = int(entry["id"])
            if image_id in seen:
                raise WorkbenchError(
                    ErrorCode.META,
                    "duplicate image id",
                    filename=filename_for_error,
                    field="id",
                )
            seen.add(image_id)
            path, filename = _safe_image_path(root, entry.get("filename"), image_id)
            filename_for_error = filename
            decoded, raw = _decode_image(path, filename)
            actual_width = int(decoded.shape[1])
            actual_height = int(decoded.shape[0])
            actual_channels = 1 if decoded.ndim == 2 else int(decoded.shape[2])
            for field_name, actual, expected_key in (
                ("width", actual_width, "width"),
                ("height", actual_height, "height"),
                ("channels", actual_channels, "channels"),
            ):
                if expected_key not in entry:
                    raise WorkbenchError(
                        ErrorCode.META,
                        f"manifest is missing {field_name}",
                        filename=filename,
                        field=field_name,
                    )
                try:
                    expected = int(entry[expected_key])
                except (TypeError, ValueError) as exc:
                    raise WorkbenchError(
                        ErrorCode.META,
                        f"manifest {field_name} must be an integer",
                        filename=filename,
                        field=field_name,
                    ) from exc
                if actual != expected:
                    raise WorkbenchError(
                        ErrorCode.META,
                        f"decoded {field_name}={actual} does not match manifest {field_name}={expected}",
                        filename=filename,
                        field=field_name,
                        details={"actual": actual, "expected": expected},
                    )
            raw_h = entry.get("homography_to_base")
            try:
                homography = normalize_homography(np.asarray(raw_h, dtype=np.float64))
            except (TypeError, ValueError) as exc:
                raise WorkbenchError(
                    ErrorCode.META,
                    "homography_to_base must be a numeric 3x3 matrix",
                    filename=filename,
                    field="homography_to_base",
                ) from exc
            except WorkbenchError as exc:
                raise WorkbenchError(
                    exc.code,
                    exc.message,
                    filename=filename,
                    field="homography_to_base",
                ) from exc
            overlap = entry.get("overlap_with_base", [])
            if not isinstance(overlap, list):
                raise WorkbenchError(
                    ErrorCode.META,
                    "overlap_with_base must be an array",
                    filename=filename,
                    field="overlap_with_base",
                )
            sha = hashlib.sha256(raw).hexdigest()
            valid_images[image_id] = ImageMeta(
                index=image_id,
                filename=filename,
                abs_path=path,
                width=actual_width,
                height=actual_height,
                channels=actual_channels,
                homography_to_base=tuple(float(v) for v in homography.reshape(-1)),
                sha256=sha,
                overlap_with_base=tuple(overlap),
                synthetic_exposure={
                    key: float(value)
                    for key, value in entry.get("synthetic_exposure", {}).items()
                    if isinstance(value, (int, float))
                },
            )
            file_hashes.append((filename, sha))
        except WorkbenchError as exc:
            if not exc.filename:
                exc.filename = filename_for_error
            errors.append(exc)
            if isinstance(image_id, int):
                failed_by_id[image_id] = exc
            if strict:
                raise
        except (KeyError, TypeError, ValueError) as exc:
            error = WorkbenchError(
                ErrorCode.META,
                f"malformed image entry: {exc}",
                filename=filename_for_error,
                field="images",
            )
            errors.append(error)
            if strict:
                raise error from exc

    if base_index not in valid_images:
        if base_index in failed_by_id:
            cause = failed_by_id[base_index]
            raise WorkbenchError(
                cause.code,
                f"base image failed validation: {cause.message}",
                filename=cause.filename,
                field=cause.field,
                details={"cause": cause.to_dict()},
            ) from cause
        raise WorkbenchError(
            ErrorCode.META,
            "base image is missing or failed validation",
            filename=valid_images.get(base_index, None).filename
            if base_index in valid_images
            else f"images[{base_index}]",
            field="base_image",
            details={"errors": [error.to_dict() for error in errors]},
        )
    if len(valid_images) < 2:
        raise WorkbenchError(
            ErrorCode.META,
            "at least two valid images are required",
            field="images",
            details={"errors": [error.to_dict() for error in errors]},
        )

    dataset_hash = hashlib.sha256()
    dataset_hash.update(raw_manifest)
    for filename, sha in sorted(file_hashes):
        dataset_hash.update(filename.encode("utf-8"))
        dataset_hash.update(sha.encode("ascii"))
    return Dataset(
        root=root,
        manifest_path=manifest_path,
        manifest=manifest,
        name=str(manifest.get("dataset_name", root.name)),
        seed=int(manifest.get("seed", 0)),
        base_index=base_index,
        theoretical_width=theoretical_width,
        theoretical_height=theoretical_height,
        theoretical_origin=(origin_x, origin_y),
        images=valid_images,
        errors=errors,
        manifest_sha256=manifest_sha,
        content_hash=dataset_hash.hexdigest(),
    )


def pair_content_hash(
    dataset: Dataset,
    image_a: int,
    image_b: int,
    parameter_version: str,
) -> str:
    meta_a = dataset.images[image_a]
    meta_b = dataset.images[image_b]
    payload = "|".join(
        [
            f"{image_a}:{meta_a.sha256}",
            f"{image_b}:{meta_b.sha256}",
            parameter_version,
            dataset.content_hash,
        ]
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()
