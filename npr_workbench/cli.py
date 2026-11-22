"""Command line entry points for generation, verification and rendering."""

from __future__ import annotations

import argparse
import json
import statistics
from pathlib import Path
from typing import Any

import numpy as np

from .consistency import save_ppm
from .engine import NprEngine, RenderControl
from .errors import NprError
from .model import NprFrame, RenderParams, Snapshot
from .npr import coverage_preview, level_preview, outline_preview
from .samples import generate_all, truth_params, verify_truth
from .scene import load_scene


def _print_json(payload: Any, *, path: str | Path | None = None) -> None:
    def default(value: Any) -> Any:
        if hasattr(value, "to_dict"):
            return value.to_dict()
        if isinstance(value, np.generic):
            return value.item()
        if isinstance(value, np.ndarray):
            return value.tolist()
        raise TypeError(f"not JSON serializable: {type(value).__name__}")

    text = json.dumps(
        payload,
        indent=2,
        sort_keys=True,
        ensure_ascii=True,
        default=default,
    )
    print(text)
    if path is not None:
        output = Path(path)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(text + "\n", encoding="utf-8")


def _scene_path(data_dir: Path, name: str) -> Path:
    path = data_dir / "scenes" / f"{name}.json"
    if not path.exists():
        raise FileNotFoundError(f"scene not found: {path}")
    return path


def _sample_names(data_dir: Path) -> list[str]:
    manifest_path = data_dir / "manifest.json"
    if manifest_path.exists():
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        return [item["scene"] for item in manifest["scenes"]]
    return sorted(path.stem for path in (data_dir / "scenes").glob("*.json"))


def _save_frame(directory: Path, frame: NprFrame, label: str = "frame") -> dict[str, str]:
    directory.mkdir(parents=True, exist_ok=True)
    save_ppm(directory / f"{label}_base.ppm", frame.base_rgb)
    save_ppm(directory / f"{label}_poster.ppm", frame.poster_rgb)
    save_ppm(
        directory / f"{label}_levels.ppm",
        level_preview(frame.levels, frame.snapshot.params.levels),
    )
    save_ppm(directory / f"{label}_outline.ppm", outline_preview(frame.outline))
    save_ppm(directory / f"{label}_brush.ppm", frame.brush_rgba[..., :3])
    save_ppm(directory / f"{label}_coverage.ppm", coverage_preview(frame.coverage))
    save_ppm(directory / f"{label}_styled.ppm", frame.styled_rgb)
    np.savez(
        directory / f"{label}_tensors.npz",
        base_rgb=frame.base_rgb,
        poster_rgb=frame.poster_rgb,
        levels=frame.levels,
        outline=frame.outline.astype(np.uint8),
        brush_rgba=frame.brush_rgba,
        styled_rgb=frame.styled_rgb,
        coverage=frame.coverage.astype(np.uint8),
    )
    return {
        "base": str(directory / f"{label}_base.ppm"),
        "poster": str(directory / f"{label}_poster.ppm"),
        "levels": str(directory / f"{label}_levels.ppm"),
        "outline": str(directory / f"{label}_outline.ppm"),
        "brush": str(directory / f"{label}_brush.ppm"),
        "coverage": str(directory / f"{label}_coverage.ppm"),
        "styled": str(directory / f"{label}_styled.ppm"),
        "tensors": str(directory / f"{label}_tensors.npz"),
    }


def command_generate_samples(args: argparse.Namespace) -> int:
    manifest = generate_all(args.out)
    _print_json(manifest)
    return 0


def command_verify_truth(args: argparse.Namespace) -> int:
    data_dir = Path(args.data)
    names = _sample_names(data_dir) if args.scene == "all" else [args.scene]
    reports: list[dict[str, Any]] = []
    passed = True
    for name in names:
        scene = load_scene(_scene_path(data_dir, name))
        truth = json.loads(
            (data_dir / "truth" / f"{name}.truth.json").read_text(encoding="utf-8")
        )
        report = verify_truth(scene, truth)
        report["scene"] = name
        reports.append(report)
        passed = passed and bool(report["passed"])
    payload = {"passed": passed, "scenes": reports}
    _print_json(payload, path=args.out)
    return 0 if passed else 2


def command_benchmark(args: argparse.Namespace) -> int:
    data_dir = Path(args.data)
    scene = load_scene(_scene_path(data_dir, args.scene))
    params = truth_params()
    engine = NprEngine(
        scene,
        params,
        scene.camera_poses[0],
        persist_dir=Path(args.out) / "persist",
        diagnostics_dir=Path(args.out) / "diagnostics",
    )
    warmups = max(2, args.warmup)
    for _ in range(warmups):
        engine.render_preview()
    preview_ms: list[float] = []
    for _ in range(args.repeat):
        frame = engine.render_preview()
        preview_ms.append(frame.duration_ms)
    exact_start = __import__("time").perf_counter()
    exact = engine.render_exact()
    exact_ms = (__import__("time").perf_counter() - exact_start) * 1000.0
    report = {
        "scene": scene.name,
        "preview_resolution": [params.preview_width, params.preview_height],
        "exact_resolution": [params.exact_width, params.exact_height],
        "preview_samples": len(preview_ms),
        "preview_median_ms": statistics.median(preview_ms),
        "preview_p95_ms": float(np.percentile(preview_ms, 95)),
        "preview_max_ms": max(preview_ms),
        "preview_budget_ms": 16.0,
        "preview_budget_met": bool(float(np.percentile(preview_ms, 95)) <= 16.0),
        "exact_ms": exact_ms,
        "exact_complete": exact.complete,
        "committed_tiles": list(exact.commit_order),
    }
    _print_json(report, path=Path(args.out) / "benchmark.json")
    return 0 if report["preview_budget_met"] else 3


def _fresh_engine(
    scene: Any,
    params: RenderParams,
    root: Path,
    session: str,
) -> NprEngine:
    return NprEngine(
        scene,
        params,
        scene.camera_poses[0],
        persist_dir=root / "persist" / session,
        diagnostics_dir=root / "diagnostics" / session,
        session_id=session,
    )


def command_verify(args: argparse.Namespace) -> int:
    data_dir = Path(args.data)
    scene = load_scene(_scene_path(data_dir, args.scene))
    params = truth_params()
    root = Path(args.out)
    root.mkdir(parents=True, exist_ok=True)

    interactive_engine = _fresh_engine(scene, params, root, "interactive")
    preview = interactive_engine.render_preview()
    interactive = interactive_engine.render_exact(preview=preview)
    if not interactive.complete or interactive.frame is None:
        raise RuntimeError("interactive exact render did not complete")

    resumed_engine = _fresh_engine(scene, params, root, "resumed")
    resumed_preview = resumed_engine.render_preview()
    control = RenderControl(pause_after_commits=args.pause_after)
    first_run = resumed_engine.render_exact(preview=resumed_preview, control=control)
    if first_run.complete:
        raise RuntimeError("resume scenario unexpectedly completed before pause")
    resumed = resumed_engine.resume(first_run.snapshot)
    if not resumed.complete or resumed.frame is None:
        raise RuntimeError("resumed exact render did not complete")

    one_shot_engine = _fresh_engine(scene, params, root, "one-shot")
    snapshot = Snapshot(
        version=preview.version,
        content_hash=scene.content_hash(),
        camera=preview.snapshot.camera,
        params=params,
    )
    one_shot = one_shot_engine.render_one_shot(snapshot=snapshot)

    path_report = interactive_engine.verify_paths(
        interactive.frame,
        resumed.frame,
        one_shot,
        diagnostic_dir=root / "diagnostics" / "paths",
    )
    saved = {
        "interactive": _save_frame(root / "renders", interactive.frame, "interactive"),
        "resumed": _save_frame(root / "renders", resumed.frame, "resumed"),
        "one_shot": _save_frame(root / "renders", one_shot, "one_shot"),
    }
    payload = {
        "passed": bool(path_report["passed"]),
        "scene": scene.name,
        "scene_hash": scene.content_hash(),
        "snapshot": snapshot.to_dict(),
        "preview": {
            "duration_ms": preview.duration_ms,
            "resolution": [preview.width, preview.height],
            "stroke_count": preview.stroke_count,
            "stroke_fingerprint_root": preview.stroke_fingerprint_root,
        },
        "interactive_exact": {
            "duration_ms": interactive.frame.duration_ms,
            "commit_order": list(interactive.commit_order),
            "failed_tiles": list(interactive.failed_tiles),
        },
        "resume": {
            "paused_after_tiles": list(first_run.committed_tiles),
            "first_commit_order": list(first_run.commit_order),
            "final_commit_order": list(resumed.commit_order),
            "checkpoint_dir": resumed.checkpoint_dir,
        },
        "one_shot": {
            "duration_ms": one_shot.duration_ms,
            "commit_order": list(one_shot.tile_commit_log),
        },
        "path_report": path_report,
        "artifacts": saved,
    }
    _print_json(payload, path=root / "verification.json")
    return 0 if payload["passed"] else 4


def command_render(args: argparse.Namespace) -> int:
    data_dir = Path(args.data)
    scene = load_scene(_scene_path(data_dir, args.scene))
    params = truth_params().with_controls(
        levels=args.levels,
        density=args.density,
        length=args.length,
    )
    if args.camera < 0 or args.camera >= len(scene.camera_poses):
        raise ValueError(f"camera index out of range: {args.camera}")
    camera = scene.camera_poses[args.camera]
    engine = NprEngine(
        scene,
        params,
        camera,
        persist_dir=Path(args.out) / "persist",
        diagnostics_dir=Path(args.out) / "diagnostics",
    )
    preview = engine.render_preview()
    exact = engine.render_exact(preview=preview)
    if exact.frame is None:
        raise RuntimeError("exact render did not produce a frame")
    artifacts = _save_frame(Path(args.out), exact.frame)
    payload = {
        "passed": True,
        "scene": scene.name,
        "camera": camera.id,
        "version": exact.snapshot.version,
        "preview_duration_ms": preview.duration_ms,
        "exact_duration_ms": exact.frame.duration_ms,
        "params": params.to_dict(),
        "artifacts": artifacts,
    }
    _print_json(payload, path=Path(args.out) / "render.json")
    return 0


def command_ui(args: argparse.Namespace) -> int:
    from .ui import run_app

    return run_app(
        data_dir=Path(args.data),
        scene_name=args.scene,
        persist_dir=Path(args.persist),
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="npr-workbench")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate-samples", help="write deterministic scenes and truth")
    generate.add_argument("--out", default="data")
    generate.set_defaults(func=command_generate_samples)

    verify_truth_parser = sub.add_parser("verify-truth", help="verify analytic sample truth")
    verify_truth_parser.add_argument("--data", default="data")
    verify_truth_parser.add_argument("--scene", default="all")
    verify_truth_parser.add_argument("--out", default=None)
    verify_truth_parser.set_defaults(func=command_verify_truth)

    benchmark = sub.add_parser("benchmark", help="measure preview latency and exact render")
    benchmark.add_argument("--data", default="data")
    benchmark.add_argument("--scene", default="orb_and_crate")
    benchmark.add_argument("--repeat", type=int, default=40)
    benchmark.add_argument("--warmup", type=int, default=5)
    benchmark.add_argument("--out", default="artifacts/benchmark")
    benchmark.set_defaults(func=command_benchmark)

    verify = sub.add_parser("verify", help="verify preview/exact and three exact paths")
    verify.add_argument("--data", default="data")
    verify.add_argument("--scene", default="orb_and_crate")
    verify.add_argument("--pause-after", type=int, default=4)
    verify.add_argument("--out", default="artifacts/verification")
    verify.set_defaults(func=command_verify)

    render = sub.add_parser("render", help="render one scene camera to PPM/NPZ artifacts")
    render.add_argument("--data", default="data")
    render.add_argument("--scene", default="orb_and_crate")
    render.add_argument("--camera", type=int, default=0)
    render.add_argument("--levels", type=int, default=5)
    render.add_argument("--density", type=float, default=0.34)
    render.add_argument("--length", type=float, default=12.0)
    render.add_argument("--out", default="artifacts/renders")
    render.set_defaults(func=command_render)

    ui = sub.add_parser("ui", help="launch the Tk desktop workbench")
    ui.add_argument("--data", default="data")
    ui.add_argument("--scene", default="orb_and_crate")
    ui.add_argument("--persist", default="persist")
    ui.set_defaults(func=command_ui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except NprError as exc:
        _print_json(exc.to_dict())
        return 2
    except (FileNotFoundError, ValueError, RuntimeError) as exc:
        _print_json({"code": "E_RUNTIME", "message": str(exc)})
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

