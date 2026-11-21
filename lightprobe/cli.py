from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .bake import Baker
from .bake_types import BakeConfig
from .coordinator import SceneRuntime
from .errors import ProbeError
from .samples import generate_samples
from .scene import load_scene
from .snapshot import load_snapshot
from .ui import serve
from .verify import compare_to_baseline, verify_two_levels


def _common_config(args: argparse.Namespace) -> dict[str, Any]:
    values = {
        "spacing": args.spacing,
        "sh_order": args.sh_order,
        "direction_count": args.directions,
        "direction_seed": args.direction_seed,
        "compensation_strength": args.compensation,
        "strategy": args.strategy,
        "audit_direction_count": args.audit_directions,
        "chunk_size": args.chunk_size,
        "validation_probe_limit": args.validation_probes,
        "validation_direction_count": args.validation_directions,
    }
    return {key: value for key, value in values.items() if value is not None}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="local-lightprobe")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate-samples", help="generate deterministic local scenes")
    generate.add_argument("--out", default="samples")

    bake_parser = subparsers.add_parser("bake", help="bake one scene")
    bake_parser.add_argument("--scene", required=True)
    bake_parser.add_argument("--snapshot-dir")
    bake_parser.add_argument("--version", type=int, default=1)
    bake_parser.add_argument("--spacing", type=float)
    bake_parser.add_argument("--sh-order", type=int)
    bake_parser.add_argument("--directions", type=int)
    bake_parser.add_argument("--direction-seed", type=int)
    bake_parser.add_argument("--compensation", type=float)
    bake_parser.add_argument("--strategy", choices=["auto", "dense", "sparse"])
    bake_parser.add_argument("--audit-directions", type=int)
    bake_parser.add_argument("--chunk-size", type=int)
    bake_parser.add_argument("--validation-probes", type=int)
    bake_parser.add_argument("--validation-directions", type=int)
    bake_parser.add_argument("--baseline")
    bake_parser.add_argument("--report")

    recover_parser = subparsers.add_parser("recover", help="resume from committed snapshot chunks")
    recover_parser.add_argument("--scene", required=True)
    recover_parser.add_argument("--snapshot-dir", required=True)
    recover_parser.add_argument("--report")

    serve_parser = subparsers.add_parser("serve", help="run the embedded 3D web UI")
    serve_parser.add_argument("--scene", required=True)
    serve_parser.add_argument("--host", default="127.0.0.1")
    serve_parser.add_argument("--port", type=int, default=8765)
    serve_parser.add_argument("--snapshot-root", default="artifacts/runs")
    serve_parser.add_argument("--no-initial-bake", action="store_true")
    serve_parser.add_argument("--spacing", type=float)
    serve_parser.add_argument("--sh-order", type=int)
    serve_parser.add_argument("--directions", type=int)
    serve_parser.add_argument("--direction-seed", type=int)
    serve_parser.add_argument("--compensation", type=float)
    serve_parser.add_argument("--strategy", choices=["auto", "dense", "sparse"])
    serve_parser.add_argument("--audit-directions", type=int)
    serve_parser.add_argument("--chunk-size", type=int)
    serve_parser.add_argument("--validation-probes", type=int)
    serve_parser.add_argument("--validation-directions", type=int)

    verify_parser = subparsers.add_parser("verify", help="run two-level acceptance verification")
    verify_parser.add_argument("--samples", default="samples")
    verify_parser.add_argument("--report", default="artifacts/reports/verification.json")
    return parser


def _print_json(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2))


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate-samples":
            _print_json(generate_samples(args.out))
            return 0
        if args.command == "bake":
            scene = load_scene(args.scene)
            config = BakeConfig.from_scene(scene, **_common_config(args))
            result = Baker().bake(
                scene,
                config,
                snapshot_dir=args.snapshot_dir,
                target_version=args.version,
                run_id=f"cli-{scene.scene_id}",
            )
            report = result.report.to_dict()
            if args.baseline:
                report["baseline_comparison"] = compare_to_baseline(
                    result, args.baseline, scene.scene_id
                )
            if args.report:
                Path(args.report).parent.mkdir(parents=True, exist_ok=True)
                Path(args.report).write_text(
                    json.dumps(report, ensure_ascii=False, indent=2) + "\n",
                    encoding="utf-8",
                )
            _print_json(report)
            return 0
        if args.command == "recover":
            scene = load_scene(args.scene)
            recovered = load_snapshot(args.snapshot_dir)
            config = BakeConfig(**recovered.metadata["config"]).validated()
            result = Baker().bake(
                scene,
                config,
                recovered=recovered,
                snapshot_dir=args.snapshot_dir,
                target_version=int(recovered.metadata["target_version"]),
                run_id=str(recovered.metadata["run_id"]),
            )
            report = result.report.to_dict()
            if args.report:
                Path(args.report).parent.mkdir(parents=True, exist_ok=True)
                Path(args.report).write_text(
                    json.dumps(report, ensure_ascii=False, indent=2) + "\n",
                    encoding="utf-8",
                )
            _print_json(report)
            return 0
        if args.command == "serve":
            scene = load_scene(args.scene)
            runtime = SceneRuntime(scene, snapshot_root=args.snapshot_root)
            overrides = _common_config(args)
            if not args.no_initial_bake:
                runtime.start_bake(config_overrides=overrides)
                runtime.wait(timeout=120)
                status = runtime.status()
                if status["last_error"]:
                    _print_json(status["last_error"])
            serve(runtime, args.host, args.port)
            return 0
        if args.command == "verify":
            report = verify_two_levels(args.samples, output_path=args.report)
            _print_json(report)
            return 0 if report["ok"] else 1
    except ProbeError as exc:
        _print_json(exc.to_dict())
        return 2
    return 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())

