from __future__ import annotations

import hashlib
import json
from pathlib import Path

import numpy as np

from .engine import EngineResult
from .errors import WorkbenchError


def coverage_pixel_hash(result: EngineResult) -> str:
    digest = hashlib.sha256()
    digest.update(result.raster.coverage_counts.tobytes())
    digest.update(
        f"{result.raster.width}x{result.raster.height}".encode("ascii")
    )
    return digest.hexdigest()


def export_result(result: EngineResult, output_base: str | Path) -> dict[str, str]:
    base = Path(output_base)
    base.parent.mkdir(parents=True, exist_ok=True)
    pgm_path = base.with_suffix(".pgm")
    npz_path = base.with_suffix(".npz")
    json_path = base.with_suffix(".json")
    png_path = base.with_suffix(".png")
    try:
        header = f"P5\n{result.raster.width} {result.raster.height}\n255\n".encode(
            "ascii"
        )
        pgm_path.write_bytes(header + result.raster.coverage_u8.tobytes())
        np.savez_compressed(
            npz_path,
            coverage_counts=result.raster.coverage_counts,
            coverage=result.raster.coverage,
            coverage_u8=result.raster.coverage_u8,
            bbox=np.asarray(result.raster.spec.bbox, dtype=np.float64),
            commit_sequence=np.asarray(result.raster.commit_sequence, dtype=np.int64),
            width=np.asarray(result.raster.width, dtype=np.int64),
            height=np.asarray(result.raster.height, dtype=np.int64),
        )
        metadata = result.summary()
        metadata["coverage_pixel_sha256"] = coverage_pixel_hash(result)
        metadata["bbox"] = list(result.raster.spec.bbox)
        json_path.write_text(
            json.dumps(metadata, ensure_ascii=True, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    except OSError as exc:
        raise WorkbenchError(
            "E_IO", f"cannot export raster result: {exc}", path=str(base)
        ) from exc
    paths = {"pgm": str(pgm_path), "npz": str(npz_path), "json": str(json_path)}
    try:
        from PySide6.QtGui import QImage

        image = QImage(
            result.raster.width,
            result.raster.height,
            QImage.Format.Format_Grayscale8,
        )
        data = result.raster.coverage_u8
        for row in range(result.raster.height):
            scanline = image.scanLine(row)
            scanline[: result.raster.width] = data[row].tobytes()
        if image.save(str(png_path), "PNG"):
            paths["png"] = str(png_path)
    except Exception:  # noqa: BLE001, S110 - PNG is a best-effort extra output.
        # PGM/NPZ are the deterministic interchange outputs; PNG is a convenience.
        pass
    return paths

