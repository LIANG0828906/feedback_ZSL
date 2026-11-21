"""End-to-end preview rendering, verification, and atomic publication."""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .config import RenderConfig
from .errors import AppError, ErrorCode
from .imageio import png_bytes
from .render import (
    RenderArtifact,
    SimulatedTermination,
    compare_precision,
    render_frame,
    summary_bytes,
)
from .scene import Scene
from .seams import SeamCheck, check_seams
from .snapshot import (
    SnapshotCheckpoint,
    SnapshotLoad,
    SnapshotStore,
)


@dataclass(slots=True)
class PreviewResult:
    scene: Scene
    config: RenderConfig
    renders: dict[str, RenderArtifact]
    seam_checks: dict[str, SeamCheck]
    precision_error: dict[str, Any] | None
    absolute_error: np.ndarray | None
    transmittance_absolute_error: np.ndarray | None
    error_bound: np.ndarray | None
    summary: dict[str, Any]
    summary_raw: bytes

    @property
    def version_id(self) -> str:
        import hashlib

        return hashlib.sha256(self.summary_raw).hexdigest()[:20]


def _seam_summary(check: SeamCheck, tolerance: float) -> dict[str, Any]:
    return {
        "ok": check.ok,
        "checked_pixels": check.checked_pixels,
        "max_color_delta": check.max_color_delta,
        "max_transmittance_delta": check.max_transmittance_delta,
        "tolerance": tolerance,
    }


def render_preview(
    scene: Scene,
    config: RenderConfig,
    *,
    precision_modes: tuple[str, ...] = ("float64", "fixed"),
    verify_seams: bool = True,
    snapshot_dir: str | Path | None = None,
    resume: bool = False,
    stop_after: int | None = None,
) -> PreviewResult:
    renders: dict[str, RenderArtifact] = {}
    seam_checks: dict[str, SeamCheck] = {}
    completed_by_mode: dict[str, dict[int, Any]] = {}
    snapshot_stores: dict[str, SnapshotStore] = {}
    snapshot_loads: dict[str, SnapshotLoad] = {}

    if snapshot_dir is not None and resume:
        root = Path(snapshot_dir)
        for precision in precision_modes:
            store = SnapshotStore(root / precision)
            snapshot_stores[precision] = store
            loaded = store.load_latest(scene, config, precision)
            if loaded is not None:
                snapshot_loads[precision] = loaded
                completed_by_mode[precision] = loaded.completed

    try:
        for precision in precision_modes:
            checkpoint: SnapshotCheckpoint | None = None
            if snapshot_dir is not None:
                store = snapshot_stores.setdefault(
                    precision, SnapshotStore(Path(snapshot_dir) / precision)
                )
                checkpoint = SnapshotCheckpoint(
                    store, scene, config, precision
                )
            artifact = render_frame(
                scene,
                config,
                precision,
                completed=completed_by_mode.get(precision),
                checkpoint=checkpoint,
                stop_after=stop_after,
            )
            if verify_seams:
                seam_checks[precision] = check_seams(artifact)
            renders[precision] = artifact
    except SimulatedTermination as exc:
        raise AppError(
            ErrorCode.E_INTERRUPTED,
            str(exc),
            field="stop_after",
            details={
                "snapshot_dir": (
                    str(snapshot_dir)
                    if snapshot_dir is not None
                    else None
                ),
                "color_output_written": False,
            },
        ) from exc

    precision_error = None
    absolute_error = None
    transmittance_absolute_error = None
    error_bound = None
    if "float64" in renders and "fixed" in renders:
        (
            precision_error,
            absolute_error,
            transmittance_absolute_error,
            error_bound,
        ) = compare_precision(scene, renders["float64"], renders["fixed"])

    summary = {
        "preview_version": 1,
        "scene": {
            "scene_id": scene.scene_id,
            "fingerprint": scene.fingerprint,
            "density_version": scene.density_version,
            "width": scene.width,
            "height": scene.height,
        },
        "config": config.to_dict(),
        "config_digest": config.digest(),
        "precision_modes": list(renders),
        "renders": {
            precision: artifact.summary
            for precision, artifact in renders.items()
        },
        "seam": {
            precision: _seam_summary(check, config.seam_tolerance)
            for precision, check in seam_checks.items()
        },
        "precision_error": precision_error,
    }
    raw = summary_bytes(summary)
    return PreviewResult(
        scene=scene,
        config=config,
        renders=renders,
        seam_checks=seam_checks,
        precision_error=precision_error,
        absolute_error=absolute_error,
        transmittance_absolute_error=transmittance_absolute_error,
        error_bound=error_bound,
        summary=summary,
        summary_raw=raw,
    )


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _write_file(path: Path, raw: bytes) -> None:
    with path.open("wb") as handle:
        handle.write(raw)
        handle.flush()
        os.fsync(handle.fileno())


def write_preview_outputs(
    output_dir: str | Path, preview: PreviewResult
) -> dict[str, Any]:
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    versions = root / "versions"
    versions.mkdir(parents=True, exist_ok=True)
    version_id = preview.version_id
    final = versions / version_id
    staging = Path(
        tempfile.mkdtemp(prefix=f".{version_id}.", dir=versions)
    )
    staging_is_published = False
    try:
        files: list[str] = []
        for precision, artifact in preview.renders.items():
            color_name = f"color_{precision}.bin"
            trans_name = f"transmittance_{precision}.bin"
            _write_file(staging / color_name, artifact.color_bytes)
            _write_file(
                staging / trans_name,
                artifact.transmittance_bytes,
            )
            files.extend([color_name, trans_name])
            _write_file(
                staging / f"summary_{precision}.json",
                summary_bytes(artifact.summary),
            )
            files.append(f"summary_{precision}.json")
        if preview.absolute_error is not None:
            _write_file(
                staging / "precision_abs_error.bin",
                np.ascontiguousarray(
                    preview.absolute_error, dtype="<f8"
                ).tobytes(),
            )
            files.append("precision_abs_error.bin")
        if preview.error_bound is not None:
            _write_file(
                staging / "precision_error_bound.bin",
                np.ascontiguousarray(
                    preview.error_bound, dtype="<f8"
                ).tobytes(),
            )
            files.append("precision_error_bound.bin")
        if preview.transmittance_absolute_error is not None:
            _write_file(
                staging / "precision_transmittance_abs_error.bin",
                np.ascontiguousarray(
                    preview.transmittance_absolute_error, dtype="<f8"
                ).tobytes(),
            )
            files.append("precision_transmittance_abs_error.bin")
        preferred_precision = (
            "fixed" if "fixed" in preview.renders else "float64"
        )
        preview_scale = max(
            1,
            min(
                8,
                512 // max(1, preview.scene.width, preview.scene.height),
            ),
        )
        _write_file(
            staging / "preview.png",
            png_bytes(
                preview.renders[preferred_precision].color,
                scale=preview_scale,
            ),
        )
        files.append("preview.png")
        _write_file(staging / "preview_summary.json", preview.summary_raw)
        files.append("preview_summary.json")
        manifest = {
            "version_id": version_id,
            "files": sorted(files),
            "color_sha256": {
                precision: artifact.summary["buffer_digest"]
                for precision, artifact in preview.renders.items()
            },
        }
        _write_file(
            staging / "manifest.json",
            summary_bytes(manifest),
        )
        _fsync_directory(staging)
        if final.exists():
            # Deterministic rerender produced the same version id. Keep the
            # already complete version and discard the staging copy.
            for child in staging.iterdir():
                child.unlink()
            staging.rmdir()
        else:
            os.replace(staging, final)
            staging_is_published = True
        _fsync_directory(versions)

        current_temp = root / f".current.tmp.{os.getpid()}"
        current_temp.unlink(missing_ok=True)
        os.symlink(Path("versions") / version_id, current_temp)
        os.replace(current_temp, root / "current")
        current_pointer = root / f".CURRENT.tmp.{os.getpid()}"
        _write_file(current_pointer, (version_id + "\n").encode("ascii"))
        os.replace(current_pointer, root / "CURRENT")
        _fsync_directory(root)
    finally:
        if (
            not staging_is_published
            and staging.exists()
            and staging.parent == versions
        ):
            for child in staging.iterdir():
                child.unlink()
            staging.rmdir()
    return {
        "ok": True,
        "version_id": version_id,
        "output_dir": str(root),
        "current": str(root / "current"),
        "summary": str(root / "current" / "preview_summary.json"),
        "summary_bytes": preview.summary_raw,
    }
