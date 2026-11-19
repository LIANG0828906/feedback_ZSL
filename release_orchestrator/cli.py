"""Command-line entry points for generation, validation, execution, and serving."""

from __future__ import annotations

import argparse
import json
import logging
import sys
from pathlib import Path
from typing import Any

from .api import serve
from .errors import BusinessError, PlanValidationError
from .generator import DEFAULT_SEED, SCENARIOS, generate_scenario
from .plan import load_plan
from .scenario import run_scenario
from .service import Orchestrator


def _add_generator_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--scenario", choices=sorted(SCENARIOS), default="mixed")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--force", action="store_true")
    parser.add_argument("--breach-batch", type=int, default=2)
    parser.add_argument("--breach-instance")
    parser.add_argument(
        "--breach-metric",
        choices=["error_rate", "connection_reject_rate"],
        default="connection_reject_rate",
    )
    parser.add_argument("--missing-batch", type=int, default=2)
    parser.add_argument("--missing-instance")
    parser.add_argument("--unapplied-instance")
    parser.add_argument("--shared-instance-count", type=int, default=5)
    parser.add_argument("--observation-seconds", type=int, default=10)
    parser.add_argument("--minimum-samples", type=int, default=5)


def _generator_kwargs(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "scenario": args.scenario,
        "seed": args.seed,
        "force": args.force,
        "breach_batch": args.breach_batch,
        "breach_instance": args.breach_instance,
        "breach_metric": args.breach_metric,
        "missing_batch": args.missing_batch,
        "missing_instance": args.missing_instance,
        "unapplied_instance": args.unapplied_instance,
        "shared_instance_count": args.shared_instance_count,
        "observation_seconds": args.observation_seconds,
        "minimum_samples": args.minimum_samples,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="release-orchestrator",
        description="Deterministic gray-release batch orchestrator",
    )
    parser.add_argument(
        "--log-level",
        default="INFO",
        choices=["DEBUG", "INFO", "WARNING", "ERROR"],
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser(
        "generate", help="generate a deterministic local sample directory"
    )
    generate_parser.add_argument("--output", required=True)
    _add_generator_arguments(generate_parser)

    scenario_parser = subparsers.add_parser(
        "scenario", help="generate and run one end-to-end scenario"
    )
    scenario_parser.add_argument("--output", required=True)
    _add_generator_arguments(scenario_parser)

    validate_parser = subparsers.add_parser(
        "validate", help="validate a plan without creating release state"
    )
    validate_parser.add_argument("--plan", required=True)

    run_parser = subparsers.add_parser(
        "run", help="start or resume a release and advance its batches"
    )
    run_parser.add_argument("--samples", default="samples")
    run_parser.add_argument("--plan")
    run_parser.add_argument("--metrics")
    run_parser.add_argument("--version-reports")
    run_parser.add_argument("--db", default="orchestrator.db")
    run_parser.add_argument("--report")
    run_parser.add_argument("--max-batches", type=int)
    run_parser.add_argument(
        "--verify-rollback",
        action="store_true",
        help="read-only verification of rollback version propagation",
    )

    serve_parser = subparsers.add_parser(
        "serve", help="start JSON and Prometheus HTTP endpoints"
    )
    serve_parser.add_argument("--host", default="127.0.0.1")
    serve_parser.add_argument("--port", type=int, default=8080)
    serve_parser.add_argument("--db", default="orchestrator.db")
    serve_parser.add_argument("--samples", default="samples")

    status_parser = subparsers.add_parser(
        "status", help="print persisted release status"
    )
    status_parser.add_argument("--db", default="orchestrator.db")
    status_parser.add_argument("--samples", default="samples")
    status_parser.add_argument("--release-id", required=True)
    status_parser.add_argument("--audit", action="store_true")

    return parser


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    logging.basicConfig(
        level=getattr(logging, args.log_level),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    try:
        if args.command == "generate":
            result = generate_scenario(args.output, **_generator_kwargs(args))
            _print_json(result)
            return 0

        if args.command == "validate":
            plan = load_plan(args.plan)
            _print_json(
                {
                    "ok": True,
                    "valid": True,
                    "plan": plan.to_dict(),
                }
            )
            return 0

        if args.command == "scenario":
            report = run_scenario(args.output, **_generator_kwargs(args))
            _print_json(report)
            return 0

        if args.command == "serve":
            orchestrator = Orchestrator(args.db, samples_dir=args.samples)
            serve(args.host, args.port, orchestrator)
            return 0

        if args.command == "run":
            orchestrator = Orchestrator(args.db, samples_dir=args.samples)
            if args.plan:
                started = orchestrator.start_release(
                    args.plan,
                    metrics_path=args.metrics,
                    version_reports_path=args.version_reports,
                    sample_dir=args.samples,
                )
            else:
                started = orchestrator.start_from_sample_dir(
                    args.samples,
                    metrics_path=args.metrics,
                    version_reports_path=args.version_reports,
                )
            release_id = str(started["release_id"])
            status = orchestrator.run_release(
                release_id, max_batches=args.max_batches
            )
            if args.verify_rollback and status["rollback"]["required"]:
                status = orchestrator.verify_rollback(release_id)
            report = orchestrator.release_report(release_id)
            report["status_detail"] = status
            report["audit"] = orchestrator.get_audit(release_id)
            if args.report:
                output = Path(args.report)
                output.parent.mkdir(parents=True, exist_ok=True)
                output.write_text(
                    json.dumps(
                        report,
                        ensure_ascii=False,
                        indent=2,
                        sort_keys=True,
                    )
                    + "\n",
                    encoding="utf-8",
                )
            _print_json(
                {
                    "release_id": release_id,
                    "status": report["status"],
                    "outcome": report["outcome"],
                    "rollback_target_version": report[
                        "rollback_target_version"
                    ],
                    "ineffective_instance_count": report[
                        "ineffective_instance_count"
                    ],
                    "report": args.report,
                }
            )
            return 0

        if args.command == "status":
            orchestrator = Orchestrator(args.db, samples_dir=args.samples)
            value: Any = (
                orchestrator.get_audit(args.release_id)
                if args.audit
                else orchestrator.get_status(args.release_id)
            )
            _print_json(value)
            return 0
    except PlanValidationError as error:
        _print_json(error.to_dict())
        return 2
    except BusinessError as error:
        _print_json(error.to_dict())
        return 3
    except (ValueError, FileExistsError, FileNotFoundError) as error:
        _print_json(
            {
                "ok": False,
                "error": {
                    "code": "INVALID_ARGUMENT",
                    "message": str(error),
                },
            }
        )
        return 2
    except KeyboardInterrupt:
        return 130
    return 1


if __name__ == "__main__":
    sys.exit(main())
