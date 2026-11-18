from __future__ import annotations

import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional, Sequence

from .config import Config, load_config
from .errors import BusinessError, E_CONFIG, E_INTERNAL, E_NETWORK
from .util import atomic_write_text, pretty_json_text, utc_timestamp


def _clean_directory(path: Path) -> None:
    resolved = path.resolve()
    if resolved == Path(resolved.anchor):
        raise BusinessError(E_CONFIG, "refusing to clean a filesystem root", {"path": str(resolved)})
    if resolved.exists():
        shutil.rmtree(resolved)
    resolved.mkdir(parents=True, exist_ok=True)


def _read_jsonl(path: Path) -> List[Dict[str, Any]]:
    if not path.exists():
        return []
    result: List[Dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            result.append(value)
    return result


def _tail(path: Path, lines: int = 30) -> str:
    if not path.exists():
        return ""
    content = path.read_text(encoding="utf-8", errors="replace").splitlines()
    return "\n".join(content[-lines:])


def _terminate_process(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    try:
        process.terminate()
        process.wait(timeout=3.0)
    except subprocess.TimeoutExpired:
        try:
            process.kill()
            process.wait(timeout=3.0)
        except subprocess.TimeoutExpired:
            pass
    except OSError:
        pass


def _open_process_log(path: Path, mode: str = "w"):
    path.parent.mkdir(parents=True, exist_ok=True)
    return path.open(mode, encoding="utf-8", buffering=1)


def _run_once(
    config_path: Path,
    job_dir: Path,
    total_timeout_s: float,
) -> Dict[str, Any]:
    _clean_directory(job_dir)
    logs_dir = job_dir / "logs"
    logs_dir.mkdir(parents=True, exist_ok=True)
    config = load_config(config_path, run_dir_override=job_dir)
    env = os.environ.copy()
    env.update(
        {
            "OMP_NUM_THREADS": "1",
            "MKL_NUM_THREADS": "1",
            "OPENBLAS_NUM_THREADS": "1",
            "NUMEXPR_NUM_THREADS": "1",
            "PYTHONUNBUFFERED": "1",
        }
    )
    processes: Dict[str, subprocess.Popen] = {}
    log_handles = []
    report: Dict[str, Any] = {
        "job_dir": str(job_dir),
        "config_fingerprint": config.config_fingerprint,
        "started_at_utc": utc_timestamp(),
    }
    try:
        server_log = _open_process_log(logs_dir / "server.log")
        log_handles.append(server_log)
        server_command = [
            sys.executable,
            "-m",
            "mlp_ps.server",
            "--config",
            str(config_path),
            "--run-dir",
            str(job_dir),
        ]
        processes["server"] = subprocess.Popen(
            server_command,
            cwd=str(Path.cwd()),
            env=env,
            stdout=server_log,
            stderr=subprocess.STDOUT,
        )

        ready_path = job_dir / "server_ready.json"
        ready_deadline = time.perf_counter() + config.registration_timeout_s
        while not ready_path.exists():
            if processes["server"].poll() is not None:
                raise BusinessError(
                    E_NETWORK,
                    "parameter server exited before becoming ready",
                    {
                        "exit_code": processes["server"].returncode,
                        "log": _tail(logs_dir / "server.log"),
                    },
                )
            if time.perf_counter() >= ready_deadline:
                raise BusinessError(E_NETWORK, "timed out waiting for the parameter server")
            time.sleep(0.02)
        ready_data = json.loads(ready_path.read_text(encoding="utf-8"))
        report["layout_id"] = ready_data["layout_id"]

        for shard_id in range(4):
            label = f"worker-{shard_id}"
            log = _open_process_log(logs_dir / f"{label}.log")
            log_handles.append(log)
            processes[label] = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "mlp_ps.worker",
                    "--config",
                    str(config_path),
                    "--run-dir",
                    str(job_dir),
                    "--worker-id",
                    label,
                    "--shard-id",
                    str(shard_id),
                ],
                cwd=str(Path.cwd()),
                env=env,
                stdout=log,
                stderr=subprocess.STDOUT,
            )

        fault = config.fault

        status_path = job_dir / "status.jsonl"
        metrics_path = job_dir / "metrics.jsonl"
        checkpoint_events_path = job_dir / "checkpoint_events.jsonl"
        deadline = time.perf_counter() + total_timeout_s
        replacement_spawned = False
        replacement_due: Optional[float] = None
        saw_pending = False
        saw_takeover = False
        status_after = 0
        while True:
            statuses = _read_jsonl(status_path)
            if len(statuses) != status_after:
                for status in statuses[status_after:]:
                    if status.get("pending_shards"):
                        saw_pending = True
                        if (
                            fault.enabled
                            and not replacement_spawned
                            and replacement_due is None
                        ):
                            replacement_due = (
                                time.perf_counter() + fault.replacement_delay_ms / 1000.0
                            )
                    active_ids = status.get("active_worker_ids", [])
                    if fault.enabled and fault.replacement_id in active_ids:
                        saw_takeover = True
                status_after = len(statuses)

            if (
                fault.enabled
                and not replacement_spawned
                and replacement_due is not None
                and time.perf_counter() >= replacement_due
            ):
                replacement_label = fault.replacement_id
                log = _open_process_log(logs_dir / f"{replacement_label}.log")
                log_handles.append(log)
                processes[replacement_label] = subprocess.Popen(
                    [
                        sys.executable,
                        "-m",
                        "mlp_ps.worker",
                        "--config",
                        str(config_path),
                        "--run-dir",
                        str(job_dir),
                        "--worker-id",
                        replacement_label,
                        "--shard-id",
                        str(fault.shard_id),
                        "--takeover",
                    ],
                    cwd=str(Path.cwd()),
                    env=env,
                    stdout=log,
                    stderr=subprocess.STDOUT,
                )
                replacement_spawned = True

            server_return = processes["server"].poll()
            if server_return is not None:
                break
            if time.perf_counter() >= deadline:
                raise BusinessError(
                    E_NETWORK,
                    "end-to-end training timed out",
                    {"timeout_seconds": total_timeout_s},
                )
            time.sleep(0.02)

        if processes["server"].returncode != 0:
            raise BusinessError(
                E_INTERNAL,
                "parameter server exited with a non-zero status",
                {
                    "exit_code": processes["server"].returncode,
                    "log": _tail(logs_dir / "server.log"),
                },
            )
        report["server_exit_code"] = processes["server"].returncode
        report["saw_pending_shards"] = saw_pending
        report["saw_takeover_active"] = saw_takeover
        if fault.enabled and not replacement_spawned:
            raise BusinessError(E_INTERNAL, "fault was configured but no pending shard was observed")
        if fault.enabled and not saw_takeover:
            raise BusinessError(E_INTERNAL, "replacement worker never became active")

    finally:
        for label, process in processes.items():
            if label != "server":
                try:
                    process.wait(timeout=max(5.0, config.step_timeout_s + 2.0))
                except subprocess.TimeoutExpired:
                    _terminate_process(process)
        if "server" in processes:
            _terminate_process(processes["server"])
        for handle in log_handles:
            try:
                handle.close()
            except Exception:
                pass

    worker_exit_codes = {
        label: process.returncode
        for label, process in processes.items()
        if label != "server"
    }
    report["worker_exit_codes"] = worker_exit_codes
    for label, return_code in worker_exit_codes.items():
        if return_code != 0:
            raise BusinessError(
                E_INTERNAL,
                "a worker process exited with a non-zero status",
                {
                    "worker": label,
                    "exit_code": return_code,
                    "log": _tail(logs_dir / f"{label}.log"),
                },
            )

    job_result = json.loads((job_dir / "job_result.json").read_text(encoding="utf-8"))
    if job_result.get("status") != "OK":
        raise BusinessError(
            str(job_result.get("code", E_INTERNAL)),
            str(job_result.get("message", "server job failed")),
            job_result.get("details", {}),
        )
    report["server_result"] = job_result

    control_log = _open_process_log(logs_dir / "control.log")
    log_handles.append(control_log)
    control_command = [
        sys.executable,
        "-m",
        "mlp_ps.control",
        "--config",
        str(config_path),
        "--run-dir",
        str(job_dir),
        "--output",
        str(job_dir / "control"),
    ]
    control_result = subprocess.run(
        control_command,
        cwd=str(Path.cwd()),
        env=env,
        stdout=control_log,
        stderr=subprocess.STDOUT,
        timeout=max(120.0, total_timeout_s),
        check=False,
    )
    control_log.close()
    if control_result.returncode != 0:
        raise BusinessError(
            E_INTERNAL,
            "single-process control training failed",
            {
                "exit_code": control_result.returncode,
                "log": _tail(logs_dir / "control.log"),
            },
        )
    control_manifest = json.loads(
        (job_dir / "control" / "control_result.json").read_text(encoding="utf-8")
    )
    report["control_result"] = control_manifest

    checkpoint_reports = []
    checkpoint_dirs = sorted(
        path
        for path in (job_dir / "checkpoints").glob("ckpt-*")
        if path.is_dir() and (path / ".complete").exists()
    )
    for checkpoint_path in checkpoint_dirs:
        verified = subprocess.run(
            [
                sys.executable,
                "-m",
                "mlp_ps.checkpoint",
                "verify",
                "--path",
                str(checkpoint_path),
            ],
            cwd=str(Path.cwd()),
            env=env,
            capture_output=True,
            text=True,
            timeout=60.0,
            check=False,
        )
        if verified.returncode != 0:
            raise BusinessError(
                E_INTERNAL,
                "independent checkpoint verification failed",
                {
                    "path": str(checkpoint_path),
                    "exit_code": verified.returncode,
                    "output": verified.stdout + verified.stderr,
                },
            )
        checkpoint_info = json.loads(verified.stdout)
        checkpoint_reports.append(
            {
                "path": str(checkpoint_path),
                "step": checkpoint_info["global_step"],
                "layout_id": checkpoint_info["layout_id"],
                "data_bytes": checkpoint_info["data_bytes"],
                "status": "OK",
            }
        )
    report["checkpoints"] = checkpoint_reports

    comparison_path = job_dir / "comparison.json"
    compared = subprocess.run(
        [
            sys.executable,
            "-m",
            "mlp_ps.compare",
            "--server",
            str(job_dir / "final"),
            "--control",
            str(job_dir / "control"),
            "--atol",
            "1e-5",
            "--output",
            str(comparison_path),
        ],
        cwd=str(Path.cwd()),
        env=env,
        capture_output=True,
        text=True,
        timeout=60.0,
        check=False,
    )
    if compared.returncode not in (0, 1) or not comparison_path.exists():
        raise BusinessError(
            E_INTERNAL,
            "parameter comparison subprocess failed",
            {
                "exit_code": compared.returncode,
                "output": compared.stdout + compared.stderr,
            },
        )
    comparison = json.loads(comparison_path.read_text(encoding="utf-8"))
    report["comparison"] = comparison

    metrics = _read_jsonl(metrics_path)
    round_ms = [float(item["round_ms"]) for item in metrics if "round_ms" in item]
    checkpoint_events = _read_jsonl(checkpoint_events_path)
    successful_checkpoint_events = [
        event for event in checkpoint_events if event.get("status") == "OK"
    ]
    bandwidth_ok = all(
        bool(event.get("within_bandwidth_limit"))
        and float(event.get("effective_bytes_per_second", 0.0))
        <= float(event.get("target_bytes_per_second", 1.0)) * 1.05
        for event in successful_checkpoint_events
    )
    avg_round_ms = sum(round_ms) / len(round_ms) if round_ms else 0.0
    report["performance"] = {
        "round_count": len(round_ms),
        "average_round_ms": round(avg_round_ms, 6),
        "max_round_ms": round(max(round_ms), 6) if round_ms else 0.0,
        "average_round_within_150ms": avg_round_ms <= 150.0,
        "max_checkpoint_capture_ms": job_result.get("max_checkpoint_capture_ms", 0.0),
        "checkpoint_capture_within_step_timeout": float(
            job_result.get("max_checkpoint_capture_ms", 0.0)
        )
        <= config.step_timeout_s * 1000.0,
        "checkpoint_event_count": len(successful_checkpoint_events),
        "checkpoint_bandwidth_within_limit": bandwidth_ok,
        "checkpoint_events": successful_checkpoint_events,
    }
    initial_mse = float(job_result.get("initial_eval_mse", 0.0))
    final_mse = float(job_result.get("final_eval_mse", 0.0))
    report["precision"] = {
        "initial_eval_mse": initial_mse,
        "final_eval_mse": final_mse,
        "final_not_worse_than_initial": final_mse <= initial_mse,
        "control_final_eval_mse": float(control_manifest.get("final_eval_mse", 0.0)),
        "mse_matches_control": abs(
            final_mse - float(control_manifest.get("final_eval_mse", 0.0))
        )
        <= 1e-7,
    }
    report["status"] = "PASS" if (
        comparison["status"] == "PASS"
        and avg_round_ms <= 150.0
        and bandwidth_ok
        and report["performance"]["checkpoint_capture_within_step_timeout"]
        and report["precision"]["final_not_worse_than_initial"]
        and report["precision"]["mse_matches_control"]
        and bool(checkpoint_reports)
        and (not fault.enabled or (saw_pending and saw_takeover))
    ) else "FAIL"
    report["finished_at_utc"] = utc_timestamp()
    return report


def run_demo(
    config_path: str | Path,
    run_dir_override: Optional[str] = None,
    repeat: int = 1,
    total_timeout_s: float = 240.0,
) -> int:
    path = Path(config_path).resolve()
    base_config = load_config(path, run_dir_override=run_dir_override)
    if repeat < 1:
        raise BusinessError(E_CONFIG, "--repeat must be at least 1")
    root_run_dir = base_config.run_dir
    _clean_directory(root_run_dir)
    reports: List[Dict[str, Any]] = []
    status_bytes: List[bytes] = []
    for index in range(repeat):
        job_dir = root_run_dir if repeat == 1 else root_run_dir / f"repeat-{index + 1:02d}"
        report = _run_once(path, job_dir, total_timeout_s)
        reports.append(report)
        status_path = job_dir / "status.jsonl"
        status_bytes.append(status_path.read_bytes())
        if report["status"] != "PASS":
            break

    determinism = {
        "checked": repeat > 1,
        "run_count": len(status_bytes),
        "byte_identical": len(set(status_bytes)) == 1 if repeat > 1 else None,
    }
    overall_status = all(report["status"] == "PASS" for report in reports) and (
        determinism["byte_identical"] is not False
    )
    summary = {
        "status": "PASS" if overall_status else "FAIL",
        "timestamp_utc": utc_timestamp(),
        "config": str(path),
        "config_fingerprint": base_config.config_fingerprint,
        "repeat": repeat,
        "determinism": determinism,
        "runs": reports,
    }
    atomic_write_text(root_run_dir / "verification_report.json", pretty_json_text(summary))
    concise = {
        "status": summary["status"],
        "run_dir": str(root_run_dir),
        "repeat": repeat,
        "determinism": determinism,
        "runs": [
            {
                "status": report["status"],
                "max_abs_error": report["comparison"]["max_abs_error"],
                "average_round_ms": report["performance"]["average_round_ms"],
                "checkpoint_bandwidth_within_limit": report["performance"][
                    "checkpoint_bandwidth_within_limit"
                ],
                "takeover_step": report["server_result"].get("takeover_step"),
                "repair_submissions": report["server_result"].get("repair_submissions"),
                "final_eval_mse": report["server_result"].get("final_eval_mse"),
                "initial_eval_mse": report["server_result"].get("initial_eval_mse"),
                "precision_non_regression": report["precision"][
                    "final_not_worse_than_initial"
                ],
                "checkpoint_capture_within_step_timeout": report["performance"][
                    "checkpoint_capture_within_step_timeout"
                ],
            }
            for report in reports
        ],
        "report": str(root_run_dir / "verification_report.json"),
    }
    print(pretty_json_text(concise), end="")
    return 0 if overall_status else 1


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Run the complete fault-recovery acceptance demo")
    parser.add_argument("--config", required=True)
    parser.add_argument("--run-dir")
    parser.add_argument("--repeat", type=int, default=1)
    parser.add_argument("--timeout-seconds", type=float, default=240.0)
    args = parser.parse_args(argv)
    try:
        return run_demo(
            args.config,
            run_dir_override=args.run_dir,
            repeat=args.repeat,
            total_timeout_s=args.timeout_seconds,
        )
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), file=sys.stderr, end="")
        return exc.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
