from __future__ import annotations

import argparse
import json
import os
import secrets
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import replace
from pathlib import Path
from typing import Any

from .api import OrchestratorAPI
from .config import PlanConfig, load_configuration
from .errors import RolloutError
from .injections import InjectionConfig, load_injections
from .loadgen import LoadGenerator, LoadTarget
from .orchestrator import RolloutOrchestrator
from .store import AuditStore


def _port_available(host: str, port: int) -> bool:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        sock.bind((host, port))
        return True
    except OSError:
        return False
    finally:
        sock.close()


def _allocate_ports(
    host: str,
    start: int,
    end: int,
    count: int,
    preferred: list[int] | None = None,
    excluded: set[int] | None = None,
) -> list[int]:
    excluded = excluded or set()
    allocated: list[int] = []
    span = end - start + 1
    offset = secrets.randbelow(span) if span > 0 else 0
    candidates = list(range(start + offset, end + 1)) + list(
        range(start, start + offset)
    )
    for port in candidates:
        if port in allocated or port in excluded:
            continue
        if _port_available(host, port):
            allocated.append(port)
            if len(allocated) == count:
                return allocated
    raise RuntimeError(
        f"cannot allocate {count} free ports in {start}-{end} on {host}"
    )


def _wait_instance(host: str, port: int, timeout: float = 60.0) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    url = f"http://{host}:{port}/admin/status"
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=0.5) as response:
                return json.loads(response.read().decode("utf-8"))
        except (OSError, urllib.error.URLError, json.JSONDecodeError) as exc:
            last_error = exc
            time.sleep(0.05)
    raise RuntimeError(
        f"instance {host}:{port} did not become ready: {last_error}"
    )


def _stop_processes(processes: list[subprocess.Popen[bytes]]) -> None:
    for process in reversed(processes):
        if process.poll() is not None:
            continue
        process.terminate()
        try:
            process.wait(timeout=2.0)
        except subprocess.TimeoutExpired:
            process.kill()
            try:
                process.wait(timeout=1.0)
            except subprocess.TimeoutExpired:
                pass


def _shutdown_instances(instances: list[Any], host: str) -> None:
    for instance in instances:
        try:
            request = urllib.request.Request(
                f"http://{host}:{instance.port}/admin/shutdown",
                data=b"{}",
                method="POST",
                headers={"Content-Type": "application/json"},
            )
            urllib.request.urlopen(request, timeout=0.5).read()
        except Exception:
            continue


def _prepare_workdir(path: Path, reset: bool) -> None:
    path.mkdir(parents=True, exist_ok=True)
    state_files = [
        path / "state.sqlite3",
        path / "state.sqlite3-wal",
        path / "state.sqlite3-shm",
        path / "resolved-plan.json",
    ]
    if reset:
        for item in state_files:
            try:
                item.unlink()
            except FileNotFoundError:
                pass
        return
    existing = [item for item in state_files if item.exists()]
    if existing:
        raise RolloutError(
            "SCENARIO_WORKDIR_NOT_EMPTY",
            "scenario work directory contains existing state",
            {"workdir": str(path), "files": [str(item) for item in existing]},
        )


def _resolved_config(
    config: PlanConfig,
    workdir: Path,
    report_path: Path,
    ports: dict[str, int],
    api_port: int,
) -> PlanConfig:
    instances = tuple(
        replace(instance, port=ports[instance.instance_id])
        for instance in config.instances
    )
    network = replace(config.network, orchestrator_port=api_port)
    storage = replace(
        config.storage,
        database_path=workdir / "state.sqlite3",
        report_path=report_path,
    )
    return replace(
        config,
        instances=instances,
        network=network,
        storage=storage,
    )


def _write_resolved_plan(config: PlanConfig, workdir: Path) -> None:
    value = {
        "release_id": config.release_id,
        "logical_sha256": config.logical_sha256,
        "instances": [
            {
                "id": instance.instance_id,
                "group": instance.group,
                "port": instance.port,
                "initial_version": instance.initial_version,
            }
            for instance in config.instances
        ],
        "orchestrator_port": config.network.orchestrator_port,
    }
    target = workdir / "resolved-plan.json"
    temporary = target.with_suffix(".json.tmp")
    temporary.write_text(
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary, target)


def _start_supervisor(
    config: PlanConfig,
    instance_id: str,
    injection: InjectionConfig,
) -> subprocess.Popen[bytes]:
    instance = config.instance_map()[instance_id]
    profile = injection.profiles[instance_id]
    faults_json = json.dumps(
        profile,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )
    root = str(Path(__file__).resolve().parent.parent)
    env = os.environ.copy()
    existing_pythonpath = env.get("PYTHONPATH")
    env["PYTHONPATH"] = (
        root
        if not existing_pythonpath
        else root + os.pathsep + existing_pythonpath
    )
    command = [
        sys.executable,
        "-m",
        "settlement_rollout.supervisor",
        "--instance-id",
        instance.instance_id,
        "--group",
        instance.group,
        "--initial-version",
        instance.initial_version,
        "--host",
        config.network.host,
        "--port",
        str(instance.port),
        "--backend-port-start",
        str(config.network.backend_port_range[0]),
        "--backend-port-end",
        str(config.network.backend_port_range[1]),
        "--faults-json",
        faults_json,
        "--precheck-timeout",
        str(config.network.precheck_timeout_seconds),
        "--drain-timeout",
        str(config.network.drain_timeout_seconds),
        "--request-timeout",
        str(config.network.request_timeout_seconds),
    ]
    return subprocess.Popen(
        command,
        cwd=root,
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )


def _decision_sequence(store: AuditStore, run_id: str) -> list[dict[str, Any]]:
    sequence: list[dict[str, Any]] = []
    for batch in store.get_batches(run_id):
        audit = store.audit_batch(run_id, batch["batch_id"])
        for decision in audit["decisions"]:
            sequence.append(
                {
                    "batch_id": decision["batch_id"],
                    "verdict": decision["verdict"],
                    "reason": decision["reason"],
                    "sample_count": decision["sample_count"],
                    "metrics": decision["metrics"],
                    "thresholds": decision["thresholds"],
                    "triggers": decision["triggers"],
                }
            )
    return sequence


def _rollback_sequence(store: AuditStore, run_id: str) -> list[dict[str, Any]]:
    sequence: list[dict[str, Any]] = []
    for action in store.all_rollbacks(run_id):
        sequence.append(
            {
                "batch_id": action["batch_id"],
                "instance_id": action["instance_id"],
                "from_version": action["from_version"],
                "to_version": action["to_version"],
                "status": action["status"],
                "error_code": action["error_code"],
            }
        )
    return sequence


def build_report(
    config: PlanConfig,
    injection: InjectionConfig,
    store: AuditStore,
    run_id: str,
    load_transport_errors: int,
) -> dict[str, Any]:
    summary = store.status_summary(run_id)
    run = store.get_run(run_id)
    batch_reports: list[dict[str, Any]] = []
    rollback_intervals: list[tuple[float, float]] = []
    for batch in store.get_batches(run_id):
        decision = store.latest_decision(run_id, batch["batch_id"])
        snapshot = (
            decision["metrics"]
            if decision is not None
            else store.latest_snapshot(run_id, batch["batch_id"])
        )
        audit = store.audit_batch(run_id, batch["batch_id"])
        rollback_actions = [
            {
                "instance_id": action["instance_id"],
                "from_version": action["from_version"],
                "to_version": action["to_version"],
                "status": action["status"],
                "error_code": action["error_code"],
            }
            for action in audit["rollback_actions"]
        ]
        rollback_duration = None
        if (
            batch["rollback_started_ts"] is not None
            and batch["rollback_finished_ts"] is not None
        ):
            rollback_duration = round(
                max(
                    0.0,
                    float(batch["rollback_finished_ts"])
                    - float(batch["rollback_started_ts"]),
                )
                * 1000.0,
                3,
            )
            rollback_intervals.append(
                (
                    float(batch["rollback_started_ts"]),
                    float(batch["rollback_finished_ts"]),
                )
            )
        sli = {
            key: snapshot.get(key)
            for key in (
                "sample_count",
                "sample_count_by_instance",
                "availability_percent",
                "p99_latency_ms",
                "latency_bad_percent",
                "availability_error_budget_consumed_ratio",
                "latency_error_budget_consumed_ratio",
                "error_budget_consumed_ratio",
                "error_budget_remaining_ratio",
                "switched_instance_count",
            )
        }
        precheck = batch.get("precheck", {})
        batch_reports.append(
            {
                "batch_id": batch["batch_id"],
                "sequence": batch["sequence"],
                "status": batch["status"],
                "target_version": batch["target_version"],
                "rollback_version": batch["rollback_version"],
                "precheck": {
                    "ready_instances": precheck.get("ready_instances", []),
                    "failed_instances": precheck.get("failed_instances", []),
                },
                "switched_instances": batch["target_switched_instances"],
                "remaining_switched_instances": batch["switched_instances"],
                "sli": sli,
                "decision": (
                    None
                    if decision is None
                    else {
                        "verdict": decision["verdict"],
                        "reason": decision["reason"],
                        "sample_count": decision["sample_count"],
                        "thresholds": decision["thresholds"],
                        "triggers": decision["triggers"],
                    }
                ),
                "rollback": {
                    "performed": bool(rollback_actions),
                    "duration_ms": rollback_duration,
                    "actions": rollback_actions,
                },
            }
        )
    if run["finished_ts"] is not None:
        release_duration = round(
            max(0.0, float(run["finished_ts"]) - float(run["created_ts"]))
            * 1000.0,
            3,
        )
    else:
        release_duration = None
    rollback_duration_total = None
    if rollback_intervals:
        rollback_duration_total = round(
            max(end for _, end in rollback_intervals)
            - min(start for start, _ in rollback_intervals),
            3,
        )
        rollback_duration_total = round(rollback_duration_total * 1000.0, 3)
    return {
        "schema_version": 1,
        "release_id": config.release_id,
        "run_id": run_id,
        "config_sha256": config.logical_sha256,
        "injection_sha256": injection.sha256,
        "expected_outcome": injection.expected_outcome,
        "outcome": summary["status"],
        "batches": batch_reports,
        "decision_sequence": _decision_sequence(store, run_id),
        "rollback_sequence": _rollback_sequence(store, run_id),
        "rollback_duration_ms": rollback_duration_total,
        "release_duration_ms": release_duration,
        "transport": {
            "connection_errors": load_transport_errors,
        },
    }


def write_report(path: Path, report: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary, path)


def run_scenario(
    plan_path: str | Path,
    slo_path: str | Path,
    injection_path: str | Path,
    workdir: str | Path,
    report_path: str | Path | None = None,
    reset: bool = True,
) -> tuple[dict[str, Any], int]:
    config = load_configuration(plan_path, slo_path)
    injection = load_injections(injection_path, config)
    work = Path(workdir).resolve()
    _prepare_workdir(work, reset=reset)
    output = (
        Path(report_path).resolve()
        if report_path is not None
        else work / "report.json"
    )
    host = config.network.host
    instance_ports = _allocate_ports(
        host,
        config.network.port_range[0],
        config.network.port_range[1],
        len(config.instances),
        preferred=[instance.port for instance in config.instances],
    )
    api_port = _allocate_ports(
        host,
        config.network.port_range[0],
        config.network.port_range[1],
        1,
        preferred=[config.network.orchestrator_port],
        excluded=set(instance_ports),
    )[0]
    ports = {
        instance.instance_id: port
        for instance, port in zip(config.instances, instance_ports)
    }
    resolved = _resolved_config(config, work, output, ports, api_port)
    _write_resolved_plan(resolved, work)

    processes: list[subprocess.Popen[bytes]] = []
    store: AuditStore | None = None
    api: OrchestratorAPI | None = None
    load: LoadGenerator | None = None
    exit_code = 0
    try:
        for instance in resolved.instances:
            processes.append(
                _start_supervisor(
                    resolved,
                    instance.instance_id,
                    injection,
                )
            )
        for instance in resolved.instances:
            _wait_instance(host, instance.port)

        store = AuditStore(resolved.storage.database_path)
        endpoints = {
            instance.instance_id: f"http://{host}:{instance.port}"
            for instance in resolved.instances
        }
        controller = RolloutOrchestrator(resolved, store, endpoints)
        controller.run_id = store.start_run(resolved)
        api = OrchestratorAPI(
            controller,
            store,
            resolved,
            host,
            resolved.network.orchestrator_port,
        )
        api.start()
        load = LoadGenerator(
            targets=[
                LoadTarget(
                    instance_id=instance.instance_id,
                    group=instance.group,
                    host=host,
                    port=instance.port,
                )
                for instance in resolved.instances
            ],
            rate_per_second=injection.rate_per_second,
            workers=injection.workers,
            timeout_seconds=resolved.network.request_timeout_seconds,
        )
        load.start()
        controller.run()
        load.stop(timeout_seconds=max(5.0, resolved.network.request_timeout_seconds))
        load_summary = load.summary()
        report = build_report(
            resolved,
            injection,
            store,
            controller.run_id,
            load_summary["connection_errors"],
        )
        write_report(output, report)
        if report["outcome"] != injection.expected_outcome:
            exit_code = 4
            print(
                json.dumps(
                    {
                        "ok": False,
                        "code": "SCENARIO_OUTCOME_MISMATCH",
                        "expected": injection.expected_outcome,
                        "actual": report["outcome"],
                        "report": str(output),
                    },
                    ensure_ascii=False,
                    sort_keys=True,
                ),
                file=sys.stderr,
            )
        else:
            print(
                json.dumps(
                    {
                        "ok": True,
                        "outcome": report["outcome"],
                        "report": str(output),
                        "run_id": report["run_id"],
                    },
                    ensure_ascii=False,
                    sort_keys=True,
                )
            )
        return report, exit_code
    except RolloutError as exc:
        raise
    finally:
        if load is not None:
            load.stop(timeout_seconds=2.0)
        if api is not None:
            api.stop()
        _shutdown_instances(list(resolved.instances), host)
        time.sleep(0.1)
        _stop_processes(processes)
        if store is not None:
            store.close()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run a reproducible local settlement rollout scenario"
    )
    parser.add_argument("--plan", required=True)
    parser.add_argument("--slo", required=True)
    parser.add_argument("--injections", required=True)
    parser.add_argument("--workdir", required=True)
    parser.add_argument("--report")
    parser.add_argument(
        "--no-reset",
        action="store_true",
        help="refuse to run when the work directory already has state",
    )
    args = parser.parse_args(argv)
    try:
        _, exit_code = run_scenario(
            plan_path=args.plan,
            slo_path=args.slo,
            injection_path=args.injections,
            workdir=args.workdir,
            report_path=args.report,
            reset=not args.no_reset,
        )
    except RolloutError as exc:
        print(
            json.dumps(exc.as_dict(), ensure_ascii=False, sort_keys=True),
            file=sys.stderr,
        )
        return exc.exit_code
    except Exception as exc:
        print(
            json.dumps(
                {
                    "ok": False,
                    "code": "SCENARIO_RUNTIME_ERROR",
                    "message": str(exc),
                },
                ensure_ascii=False,
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 3
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
