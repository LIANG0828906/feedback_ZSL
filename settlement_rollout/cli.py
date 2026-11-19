from __future__ import annotations

import argparse
import json
import sys
from typing import Any

from .api import OrchestratorAPI, render_prometheus
from .config import load_configuration
from .errors import RolloutError
from .orchestrator import RolloutOrchestrator
from .scenario import main as scenario_main
from .store import AuditStore


def _print(value: dict[str, Any], stream: Any = sys.stdout) -> None:
    print(
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2),
        file=stream,
    )


def _cmd_validate(args: argparse.Namespace) -> int:
    config = load_configuration(args.plan, args.slo)
    _print(
        {
            "ok": True,
            "release_id": config.release_id,
            "config_sha256": config.logical_sha256,
            "instances": [
                {
                    "id": instance.instance_id,
                    "group": instance.group,
                    "port": instance.port,
                    "initial_version": instance.initial_version,
                }
                for instance in config.instances
            ],
            "batches": [
                {
                    "id": batch.batch_id,
                    "order": batch.order,
                    "instances": list(batch.instances),
                    "rollback_version": batch.rollback_version,
                }
                for batch in config.batches
            ],
            "observation_window_seconds": config.observations_seconds,
            "sampling_interval_seconds": config.sampling.interval_seconds,
            "slo": {
                "availability_target_percent": (
                    config.slo.availability_target_percent
                ),
                "latency_percentile": config.slo.latency_percentile,
                "p99_target_ms": config.slo.latency_target_ms,
                "minimum_samples": config.slo.minimum_samples,
                "max_error_budget_consumed_percent": (
                    config.slo.max_error_budget_consumed_percent
                ),
            },
        }
    )
    return 0


def _cmd_serve(args: argparse.Namespace) -> int:
    config = load_configuration(args.plan, args.slo)
    store = AuditStore(
        args.database or config.storage.database_path
    )
    controller = RolloutOrchestrator(config, store)
    controller.run_id = store.start_run(config)
    api = OrchestratorAPI(
        controller,
        store,
        config,
        config.network.host,
        config.network.orchestrator_port,
    )
    api.start()
    try:
        summary = controller.run()
        _print({"ok": True, "summary": summary})
        return 0
    except KeyboardInterrupt:
        controller.abort("keyboard_interrupt")
        summary = controller.run()
        _print({"ok": True, "summary": summary})
        return 130
    finally:
        api.stop()
        store.close()


def _cmd_status(args: argparse.Namespace) -> int:
    store = AuditStore(args.database)
    try:
        _print(store.status_summary(args.run_id))
    finally:
        store.close()
    return 0


def _cmd_metrics(args: argparse.Namespace) -> int:
    config = load_configuration(args.plan, args.slo)
    store = AuditStore(args.database)
    try:
        sys.stdout.write(render_prometheus(store, config, args.run_id))
    finally:
        store.close()
    return 0


def _cmd_audit(args: argparse.Namespace) -> int:
    store = AuditStore(args.database)
    try:
        if args.batch:
            audit = store.audit_batch(args.run_id, args.batch)
            _print({"ok": True, "audit": audit})
        else:
            audits = [
                store.audit_batch(args.run_id, batch["batch_id"])
                for batch in store.get_batches(args.run_id)
            ]
            _print({"ok": True, "audits": audits})
    finally:
        store.close()
    return 0


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="settlement-rollout",
        description="Local settlement gray-release orchestrator",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    validate = subparsers.add_parser("validate", help="validate plan and SLO files")
    validate.add_argument("--plan", required=True)
    validate.add_argument("--slo", required=True)
    validate.set_defaults(func=_cmd_validate)

    scenario = subparsers.add_parser(
        "scenario",
        help="start instances, load, fault injection and execute a scenario",
    )
    scenario.add_argument("--plan", required=True)
    scenario.add_argument("--slo", required=True)
    scenario.add_argument("--injections", required=True)
    scenario.add_argument("--workdir", required=True)
    scenario.add_argument("--report")
    scenario.add_argument("--no-reset", action="store_true")
    scenario.set_defaults(func=lambda args: scenario_main([
        "--plan",
        args.plan,
        "--slo",
        args.slo,
        "--injections",
        args.injections,
        "--workdir",
        args.workdir,
        *(["--report", args.report] if args.report else []),
        *(["--no-reset"] if args.no_reset else []),
    ]))

    serve = subparsers.add_parser(
        "serve",
        help="run the orchestrator against already-running instances",
    )
    serve.add_argument("--plan", required=True)
    serve.add_argument("--slo", required=True)
    serve.add_argument("--database")
    serve.set_defaults(func=_cmd_serve)

    status = subparsers.add_parser("status", help="print JSON run summary")
    status.add_argument("--database", required=True)
    status.add_argument("--run-id", required=True)
    status.set_defaults(func=_cmd_status)

    metrics = subparsers.add_parser("metrics", help="print Prometheus metrics")
    metrics.add_argument("--plan", required=True)
    metrics.add_argument("--slo", required=True)
    metrics.add_argument("--database", required=True)
    metrics.add_argument("--run-id", required=True)
    metrics.set_defaults(func=_cmd_metrics)

    audit = subparsers.add_parser("audit", help="print audit records")
    audit.add_argument("--database", required=True)
    audit.add_argument("--run-id", required=True)
    audit.add_argument("--batch")
    audit.set_defaults(func=_cmd_audit)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except RolloutError as exc:
        _print(exc.as_dict(), sys.stderr)
        return exc.exit_code
    except FileNotFoundError as exc:
        _print(
            {
                "ok": False,
                "code": "FILE_NOT_FOUND",
                "message": str(exc),
            },
            sys.stderr,
        )
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
