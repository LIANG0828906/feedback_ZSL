"""Command line entry points for local experiments."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .canonical import atomic_write_json, canonical_json, parse_json_bytes
from .comparison import write_comparison_reports
from .engine import Engine, replay_trace
from .errors import E_SCENARIO_INVALID, KernelError
from .experiment import run_experiment
from .precision import Precision
from .samples import generate_samples
from .scenario import Scenario, load_scenario


def _print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _load_operations(path: str | None, scenario_id: str) -> list[Any]:
    if path is None:
        return []
    value = parse_json_bytes(Path(path).read_bytes())
    if isinstance(value, dict):
        value = value.get(scenario_id)
    if not isinstance(value, list):
        raise KernelError(
            E_SCENARIO_INVALID,
            "operations file must contain a JSON array or a scenario-id map",
            {"path": path, "scenario_id": scenario_id},
        )
    return value


def _select_precision(args: argparse.Namespace, scenario: Scenario) -> Precision:
    if args.mode is None:
        return scenario.precision
    return Precision(args.mode, args.places, args.rounding)


def _summary(result: Any, trace_path: str | None = None) -> dict[str, Any]:
    return {
        "ok": True,
        "scenario_id": result.engine.scenario.scenario_id,
        "precision": result.precision.to_dict(),
        "step_count": len(result.steps),
        "selected_events": [
            {
                "step_index": step["index"],
                "event_id": step["selected_event"],
                "branch_id": step["selected_branch"],
            }
            for step in result.steps
        ],
        "final_state_hash": result.final_state_hash,
        "causal_chain_hash": result.causal_chain_hash,
        "terminated_reason": result.terminated_reason,
        "trace": trace_path,
    }


def _cmd_generate(args: argparse.Namespace) -> int:
    manifest = generate_samples(args.out_dir, args.seed)
    _print({"ok": True, "manifest": manifest})
    return 0


def _cmd_run(args: argparse.Namespace) -> int:
    scenario = load_scenario(args.scenario, injection=args.inject)
    precision = _select_precision(args, scenario)
    operations = _load_operations(args.operations, scenario.scenario_id)
    if args.recover:
        if not args.snapshot_dir:
            raise KernelError(
                E_SCENARIO_INVALID,
                "--recover requires --snapshot-dir",
                {},
            )
        engine = Engine.recover(
            scenario,
            precision,
            args.snapshot_dir,
            operations=operations,
            max_steps=args.max_steps,
        )
    else:
        engine = Engine(
            scenario,
            precision,
            operations=operations,
            max_steps=args.max_steps,
            snapshot_dir=args.snapshot_dir,
            crash_after=args.crash_after,
        )
    result = engine.run()
    if args.trace:
        result.write_trace(args.trace)
    _print(_summary(result, args.trace))
    return 0


def _cmd_compare(args: argparse.Namespace) -> int:
    scenario = load_scenario(args.scenario, injection=args.inject)
    operations = _load_operations(args.operations, scenario.scenario_id)
    fixed = Engine(
        scenario,
        Precision("fixed", args.fixed_places, args.rounding),
        operations=operations,
        max_steps=args.max_steps,
    ).run()
    floating = Engine(
        scenario,
        Precision("float"),
        operations=operations,
        max_steps=args.max_steps,
    ).run()
    paths = write_comparison_reports(
        args.out_dir,
        scenario,
        fixed,
        floating,
        prefix=args.prefix,
    )
    _print(
        {
            "ok": True,
            "fixed": _summary(fixed),
            "float": _summary(floating),
            "reports": {name: str(path) for name, path in paths.items()},
        }
    )
    return 0


def _cmd_replay(args: argparse.Namespace) -> int:
    result = replay_trace(args.scenario, args.trace)
    _print(
        {
            **_summary(result),
            "ok": True,
            "replay_verified": True,
            "trace": args.trace,
        }
    )
    return 0


def _cmd_experiment(args: argparse.Namespace) -> int:
    report = run_experiment(
        data_dir=args.data_dir,
        reports_dir=args.reports_dir,
        seed=args.seed,
    )
    _print({"ok": report["ok"], "report": report})
    return 0 if report["ok"] else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="causalkernel",
        description="Deterministic causal narrative evaluation kernel",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out-dir", default="data")
    generate.add_argument("--seed", type=int, default=20261121)
    generate.set_defaults(handler=_cmd_generate)

    run = subparsers.add_parser("run", help="advance one scenario")
    run.add_argument("--scenario", required=True)
    run.add_argument("--mode", choices=["fixed", "float"])
    run.add_argument("--places", type=int, default=6)
    run.add_argument("--rounding", choices=["half_even", "half_up"], default="half_even")
    run.add_argument("--operations")
    run.add_argument("--trace")
    run.add_argument("--snapshot-dir")
    run.add_argument("--recover", action="store_true")
    run.add_argument("--max-steps", type=int, default=64)
    run.add_argument("--crash-after", type=int)
    run.add_argument("--inject")
    run.set_defaults(handler=_cmd_run)

    compare = subparsers.add_parser("compare", help="run fixed and float then report")
    compare.add_argument("--scenario", required=True)
    compare.add_argument("--operations")
    compare.add_argument("--out-dir", default="reports")
    compare.add_argument("--prefix")
    compare.add_argument("--fixed-places", type=int, default=6)
    compare.add_argument("--rounding", choices=["half_even", "half_up"], default="half_even")
    compare.add_argument("--max-steps", type=int, default=64)
    compare.add_argument("--inject")
    compare.set_defaults(handler=_cmd_compare)

    replay = subparsers.add_parser("replay", help="replay and verify a trace")
    replay.add_argument("--scenario", required=True)
    replay.add_argument("--trace", required=True)
    replay.set_defaults(handler=_cmd_replay)

    experiment = subparsers.add_parser("experiment", help="run the full local validation suite")
    experiment.add_argument("--data-dir", default="data")
    experiment.add_argument("--reports-dir", default="reports")
    experiment.add_argument("--seed", type=int, default=20261121)
    experiment.set_defaults(handler=_cmd_experiment)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except KernelError as error:
        print(json.dumps({"ok": False, "error": error.to_dict()}, ensure_ascii=False, sort_keys=True))
        return 2
    except KeyboardInterrupt:
        print(json.dumps({"ok": False, "error": {"code": "E_INTERRUPTED"}}))
        return 130


if __name__ == "__main__":
    sys.exit(main())
