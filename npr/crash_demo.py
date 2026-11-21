"""Deterministic forced-termination harness for recovery verification.

Two scripted phases are meant to run in separate processes:
  1. ``crash`` starts a recompute and hard-exits (os._exit, like SIGKILL) at a
     configured layer boundary, leaving only previously committed chunks valid;
  2. ``resume`` opens the same workdir, validates the hash chain, prunes
     uncommitted/temp files and continues to a complete new version.
"""

import argparse
import json
import os

from .canonical import read_json, write_json_atomic
from .params import params_from_dict
from .pipeline import Pipeline
from .preview import PreviewService
from .scene import load_scene

HOOK_NAME = "crash_hook.json"
PENDING_NAME = "pending_params.json"


def run_crash(workdir, scene_file, crash_layer, crash_version, outline_width=0.7):
    scene = load_scene(scene_file)
    pipeline = Pipeline(workdir)
    params = _load_or_default_params(workdir, scene_file)
    if outline_width is not None:
        params = (
            type(params[0])(**{**params[0].__dict__, "width": outline_width}),
            params[1],
            params[2],
        )
    write_json_atomic(
        os.path.join(workdir, PENDING_NAME),
        {
            "params": {
                "outline": params[0].factor(),
                "shadow": params[1].factor(),
                "smear": params[2].factor(),
            }
        },
    )
    write_json_atomic(
        os.path.join(workdir, HOOK_NAME),
        {"layer": crash_layer, "version": crash_version},
    )
    pipeline.crash_hook = read_json(os.path.join(workdir, HOOK_NAME))
    # Hard exit happens inside the recompute; nothing after this returns.
    pipeline.recompute(scene, *params)
    raise SystemExit("crash hook did not fire")


def run_resume(workdir, scene_file):
    scene = load_scene(scene_file)
    pipeline = Pipeline(workdir)
    pending_path = os.path.join(workdir, PENDING_NAME)
    if os.path.exists(pending_path):
        saved = read_json(pending_path)
        params = (
            params_from_dict(saved["params"]["outline"]),
            params_from_dict(saved["params"]["shadow"]),
            params_from_dict(saved["params"]["smear"]),
        )
    else:
        params = _load_or_default_params(workdir, scene_file)
    manifest, missing = pipeline.recover()
    before = None if manifest is None else manifest["version"]
    factors = pipeline.build_factors(scene, *params)
    stale = manifest is None or manifest["factors"] != factors
    if stale or missing:
        manifest, _ = pipeline.recompute(scene, *params)
    service = PreviewService(pipeline.store)
    snapshot, _ = service.publish(manifest)
    result = {
        "recovered_version": before,
        "completed_version": manifest["version"],
        "missing_before": missing,
        "manifest_hash": manifest["manifest_hash"],
        "preview_version": snapshot.version,
        "layer_count": len(snapshot.layers),
    }
    hook_path = os.path.join(workdir, HOOK_NAME)
    if os.path.exists(hook_path):
        os.unlink(hook_path)
    if os.path.exists(pending_path):
        os.unlink(pending_path)
    print(json.dumps(result, sort_keys=True))
    return result


def _load_or_default_params(workdir, scene_file):
    params_path = os.path.join(workdir, "params.json")
    if os.path.exists(params_path):
        saved = read_json(params_path)
        return (
            params_from_dict(saved["params"]["outline"]),
            params_from_dict(saved["params"]["shadow"]),
            params_from_dict(saved["params"]["smear"]),
        )
    # A crash version always changes at least one factor so all layers are
    # recomputed and the commit loop reaches the configured crash layer.
    from .params import OutlineParams, ShadowParams, SmearParams

    return OutlineParams(width=0.7), ShadowParams(), SmearParams()


def main(argv=None):
    parser = argparse.ArgumentParser(prog="npr-crash-demo")
    sub = parser.add_subparsers(dest="command", required=True)
    p_crash = sub.add_parser("crash")
    p_crash.add_argument("workdir")
    p_crash.add_argument("scene_file")
    p_crash.add_argument("crash_layer", type=int)
    p_crash.add_argument("crash_version", type=int)
    p_crash.add_argument("--outline-width", type=float, default=0.7)
    p_resume = sub.add_parser("resume")
    p_resume.add_argument("workdir")
    p_resume.add_argument("scene_file")
    args = parser.parse_args(argv)
    if args.command == "crash":
        run_crash(
            args.workdir,
            args.scene_file,
            args.crash_layer,
            args.crash_version,
            args.outline_width,
        )
    else:
        run_resume(args.workdir, args.scene_file)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
