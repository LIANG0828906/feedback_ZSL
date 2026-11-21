"""Scripted entry points: samples, regression runs, recovery and preview."""

import argparse
import json
import os
import subprocess
import sys

from .canonical import read_json, write_json_atomic
from .errors import PipelineError
from .params import OutlineParams, ShadowParams, SmearParams, params_from_dict
from .pipeline import Pipeline
from .preview import PreviewService
from .samples import DENSITY, PROFILES, generate_samples
from .scene import load_scene


def main(argv=None):
    parser = argparse.ArgumentParser(prog="npr")
    sub = parser.add_subparsers(dest="command", required=True)

    p_gen = sub.add_parser("gen-samples", help="generate deterministic scene samples")
    p_gen.add_argument("out_dir")
    p_gen.add_argument("--seed", type=int, default=20261122)

    p_run = sub.add_parser("regression", help="full scripted regression sequence")
    p_run.add_argument("workdir")
    p_run.add_argument("--samples", default=None)
    p_run.add_argument("--seed", type=int, default=20261122)

    p_rec = sub.add_parser("recover", help="recover/continue from disk state")
    p_rec.add_argument("workdir")
    p_rec.add_argument("scene_file")

    p_pre = sub.add_parser("preview", help="offscreen preview PNG of latest version")
    p_pre.add_argument("workdir")
    p_pre.add_argument("png_path")

    sub.add_parser("gl-probe", help="check whether a GL context can be created")

    args = parser.parse_args(argv)
    return globals()[f"_cmd_{args.command.replace('-', '_')}"](args)


def _cmd_gen_samples(args):
    index = generate_samples(args.out_dir, args.seed)
    print(json.dumps(index, sort_keys=True, indent=2))
    return 0


def _params_for(profile_name, overrides=None):
    density = DENSITY[PROFILES[profile_name]["density"]]
    outline = OutlineParams(spacing=density["outline_spacing"])
    shadow = ShadowParams(spacing=density["shadow_spacing"])
    smear = SmearParams(grid=density["smear_grid"])
    if overrides:
        if "outline" in overrides:
            outline = OutlineParams(**{**outline.__dict__, **overrides["outline"]})
        if "shadow" in overrides:
            shadow = ShadowParams(**{**shadow.__dict__, **overrides["shadow"]})
        if "smear" in overrides:
            smear = SmearParams(**{**smear.__dict__, **overrides["smear"]})
    return outline, shadow, smear


def _cmd_regression(args):
    samples_dir = args.samples or os.path.join(args.workdir, "samples")
    os.makedirs(args.workdir, exist_ok=True)
    generate_samples(samples_dir, args.seed)

    baseline = {"seed": args.seed, "runs": []}
    for profile in ("tiny", "medium", "large"):
        scene_path = os.path.join(samples_dir, f"scene_{profile}.json")
        scene = load_scene(scene_path)
        profile_workdir = os.path.join(args.workdir, f"run_{profile}")
        pipeline = Pipeline(profile_workdir)

        outline_p, shadow_p, smear_p = _params_for(profile)
        manifest, _ = pipeline.recompute(scene, outline_p, shadow_p, smear_p)
        first_hash = manifest["manifest_hash"]

        # Re-run identical inputs: nothing is affected, everything reused.
        manifest, _ = pipeline.recompute(scene, outline_p, shadow_p, smear_p)
        noop_hash = manifest["manifest_hash"]

        # Adjust only the outline layer: all layers must be invalidated.
        outline2 = OutlineParams(**{**outline_p.__dict__, "width": outline_p.width + 0.1})
        manifest, _ = pipeline.recompute(scene, outline2, shadow_p, smear_p)
        outline_change_hash = manifest["manifest_hash"]

        # Adjust only shadow spacing: shadow + smear invalidated, outline reused.
        shadow2 = ShadowParams(**{**shadow_p.__dict__, "spacing": shadow_p.spacing * 0.8})
        manifest, _ = pipeline.recompute(scene, outline2, shadow2, smear_p)
        shadow_change_hash = manifest["manifest_hash"]

        baseline["runs"].append(
            {
                "profile": profile,
                "first_manifest_hash": first_hash,
                "noop_manifest_hash": noop_hash,
                "outline_change_hash": outline_change_hash,
                "shadow_change_hash": shadow_change_hash,
                "final_version": manifest["version"],
            }
        )

    baseline_path = os.path.join(args.workdir, "baseline.json")
    write_json_atomic(baseline_path, baseline)
    print(json.dumps(baseline, sort_keys=True, indent=2))
    return 0


def _cmd_recover(args):
    scene = load_scene(args.scene_file)
    pipeline = Pipeline(args.workdir)
    params_path = os.path.join(args.workdir, "params.json")
    if not os.path.exists(params_path):
        raise PipelineError.recovery_invalid("params.json missing; cannot resume")
    saved = read_json(params_path)
    outline_p = params_from_dict(saved["params"]["outline"])
    shadow_p = params_from_dict(saved["params"]["shadow"])
    smear_p = params_from_dict(saved["params"]["smear"])
    manifest, missing = pipeline.recover()
    if manifest is None:
        print("no committed state; fresh build required")
        manifest, _ = pipeline.recompute(scene, outline_p, shadow_p, smear_p)
    elif missing:
        print(f"resuming incomplete layers {missing}")
        manifest, _ = pipeline.resume(scene, outline_p, shadow_p, smear_p)
    else:
        print(f"recovered complete version v{manifest['version']}")
    print(json.dumps({"version": manifest["version"], "hash": manifest["manifest_hash"]}))
    return 0


def _cmd_preview(args):
    pipeline = Pipeline(args.workdir)
    manifest, _missing = pipeline.recover()
    if manifest is None:
        raise PipelineError.recovery_invalid("no committed version to preview")
    service = PreviewService(pipeline.store)
    snapshot, _ = service.publish(manifest)
    image_hash = service.save_png(args.png_path, snapshot)
    print(json.dumps({"version": snapshot.version, "png": args.png_path, "hash": image_hash}))
    return 0


def _cmd_gl_probe(_args):
    import importlib

    try:
        importlib.import_module("PyQt5")
    except ImportError as exc:
        print(json.dumps({"gl": False, "reason": f"PyQt5 unavailable: {exc}"}))
        return 0
    requested = os.environ.get("QT_QPA_PLATFORM")
    candidates = [requested] if requested else ["wayland", "xcb", "offscreen"]
    info = {"gl": False, "reason": "no platform plugin worked", "offscreen_fallback": True}
    for platform in candidates:
        probe = subprocess.run(
            [
                sys.executable,
                "-c",
                "import sys; from PyQt5 import QtGui, QtWidgets;\n"
                "app=QtWidgets.QApplication([]); c=QtGui.QOpenGLContext();\n"
                "print('OK' if c.create() else 'NO', c.format().majorVersion(), c.format().minorVersion())",
            ],
            env={**os.environ, "QT_QPA_PLATFORM": platform},
            capture_output=True,
            text=True,
        )
        if probe.returncode == 0 and probe.stdout.startswith("OK"):
            parts = probe.stdout.split()
            info = {
                "gl": True,
                "platform": platform,
                "version": f"{parts[1]}.{parts[2]}",
                "offscreen_fallback": True,
            }
            break
        info = {
            "gl": False,
            "platform": platform,
            "reason": (probe.stderr.strip().splitlines() or ["plugin failed"])[-1],
            "offscreen_fallback": True,
        }
    print(json.dumps(info, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
