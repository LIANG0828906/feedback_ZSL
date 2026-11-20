"""Command-line entry points for generation, experiments, and replay checks."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .experiment import (
    generate_and_run,
    run_experiment,
    verify_replay,
    write_baseline,
)
from .samples import read_sample


def _print_json(value: object) -> None:
    print(json.dumps(value, ensure_ascii=True, indent=2, sort_keys=True))


def _summary(results: list[object], baseline_hash: str) -> dict[str, object]:
    samples = []
    for result in results:
        samples.append(
            {
                "size": result.size,
                "trace": str(result.trace_path),
                "trace_sha256": result.trace_sha256,
                "final_version_hash": result.final_version_hash,
            }
        )
    return {"baseline_sha256": baseline_hash, "samples": samples}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="tactical-cli",
        description="Deterministic tactical grid experiment runner",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="generate samples, snapshots, traces, and baselines"
    )
    generate.add_argument(
        "--output", default="artifacts/experiment", help="output directory"
    )
    generate.add_argument(
        "--sizes",
        nargs="+",
        type=int,
        default=[64, 512],
        help="square grid sizes (default: 64 512)",
    )
    generate.add_argument(
        "--seed", type=int, default=20261121, help="deterministic sample seed"
    )
    generate.add_argument(
        "--verify-twice",
        action="store_true",
        help="rerun the same samples in a replay directory and compare bytes",
    )

    run_sample = subparsers.add_parser("run-sample", help="run one generated sample")
    run_sample.add_argument("--sample", required=True)
    run_sample.add_argument("--output", required=True)

    verify = subparsers.add_parser(
        "verify", help="rerun existing samples and compare trace bytes"
    )
    verify.add_argument("--output", default="artifacts/experiment")
    verify.add_argument("--sizes", nargs="+", type=int, default=[64, 512])
    verify.add_argument("--seed", type=int, default=20261121)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.command == "generate":
        results, baseline_hash = generate_and_run(args.output, args.sizes, args.seed)
        summary: dict[str, object] = _summary(results, baseline_hash)
        if args.verify_twice:
            summary["replay"] = verify_replay(args.output, args.sizes, args.seed)
        _print_json(summary)
        return 0
    if args.command == "run-sample":
        sample = read_sample(Path(args.sample))
        result = run_experiment(sample, args.output)
        baseline_hash = write_baseline(Path(args.output) / "baseline.json", [result])
        _print_json(_summary([result], baseline_hash))
        return 0
    if args.command == "verify":
        _print_json(verify_replay(args.output, args.sizes, args.seed))
        return 0
    return 2


if __name__ == "__main__":
    sys.exit(main())
