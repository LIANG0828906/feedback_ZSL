from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import threading
import time
from pathlib import Path

from .backends import BackendError, create_backend
from .config import Config
from .coordinator import Coordinator
from .server import create_server


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="ordergroup",
        description="Kafka order-events consumer-group coordinator",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    def add_common(sub: argparse.ArgumentParser) -> None:
        sub.add_argument("--config", help="JSON configuration file")
        sub.add_argument("--host", help="HTTP listen host")
        sub.add_argument("--port", type=int, help="HTTP listen port (0 chooses a free port)")
        sub.add_argument("--db", help="SQLite database path")
        sub.add_argument(
            "--backend",
            choices=["kafka", "simulator"],
            help="message backend for this run",
        )
        sub.add_argument("--bootstrap", help="Kafka bootstrap server list")
        sub.add_argument("--topic", help="Kafka topic")
        sub.add_argument("--partitions", type=int, help="topic partition count")
        sub.add_argument("--simulator-dir", help="simulator JSONL directory")
        sub.add_argument("--quiet", action="store_true", help="suppress access logs")

    coordinator = subparsers.add_parser("coordinator", help="run the group coordinator")
    add_common(coordinator)
    coordinator.add_argument(
        "--reset-state",
        action="store_true",
        help="delete the existing SQLite state before starting",
    )

    worker = subparsers.add_parser("worker", help="run one consumer worker process")
    worker.add_argument("--config", help="JSON configuration file")
    worker.add_argument("--coordinator", default="http://127.0.0.1:8090")
    worker.add_argument("--member-id", default="worker-1")
    worker.add_argument("--session-timeout-ms", type=int)
    worker.add_argument("--heartbeat-interval-ms", type=int)
    worker.add_argument("--batch-size", type=int)
    worker.add_argument("--consume-delay-ms", type=int)
    worker.add_argument("--stop-after-events", type=int)
    worker.add_argument("--duration-seconds", type=float)
    worker.add_argument("--faults", help="fault-injection JSON file")
    worker.add_argument("--stop-heartbeat-after-ms", type=int)
    worker.add_argument("--pause-after-records", type=int)
    worker.add_argument("--pause-duration-ms", type=int)
    worker.add_argument("--pause-partition", type=int)
    worker.add_argument("--stall-after-records", type=int)
    worker.add_argument("--stall-duration-ms", type=int)
    worker.add_argument("--processed-log", help="append processed-event ledger JSONL")
    worker.add_argument(
        "--no-leave-on-exit",
        action="store_true",
        help="do not send a graceful leave when the worker exits",
    )
    worker.add_argument("--quiet", action="store_true")

    generator = subparsers.add_parser("generate", help="generate deterministic order events")
    generator.add_argument("--config", help="JSON configuration file")
    generator.add_argument("--count", type=int, default=20000)
    generator.add_argument("--seed", type=int, default=290)
    generator.add_argument("--topic", help="Kafka topic")
    generator.add_argument("--partitions", type=int, help="topic partition count")
    generator.add_argument("--backend", choices=["kafka", "simulator"], help="output backend")
    generator.add_argument("--bootstrap", help="Kafka bootstrap servers")
    generator.add_argument("--output-dir", help="simulator output directory")
    generator.add_argument("--batch-size", type=int, default=500)
    generator.add_argument("--jsonl", help="also write all events to this JSONL path")
    generator.add_argument("--dry-run", action="store_true", help="generate without writing")

    return parser


def _apply_overrides(config: Config, args: argparse.Namespace) -> None:
    mapping = {
        ("server", "host"): getattr(args, "host", None),
        ("server", "port"): getattr(args, "port", None),
        ("storage", "path"): getattr(args, "db", None),
        ("backend", "kind"): getattr(args, "backend", None),
        ("backend", "simulator_dir"): getattr(args, "simulator_dir", None),
        ("kafka", "bootstrap_servers"): getattr(args, "bootstrap", None),
        ("kafka", "topic"): getattr(args, "topic", None),
        ("kafka", "partitions"): getattr(args, "partitions", None),
    }
    for (section, key), value in mapping.items():
        if value is not None:
            config.data.setdefault(section, {})[key] = value


def _run_coordinator(args: argparse.Namespace) -> int:
    config = Config.load(args.config)
    _apply_overrides(config, args)
    if args.reset_state and config.db_path.exists():
        for suffix in ("", "-wal", "-shm"):
            candidate = Path(str(config.db_path) + suffix)
            if candidate.exists():
                candidate.unlink()
    backend = create_backend(config)
    coordinator = Coordinator(config, backend)
    coordinator.start()
    server = create_server(
        coordinator,
        config.host,
        config.port,
        quiet=bool(args.quiet),
    )
    actual_host, actual_port = server.server_address[:2]
    ready = {
        "event": "ready",
        "service": "order-group-coordinator",
        "host": actual_host,
        "port": actual_port,
        "topic": config.topic,
        "partitions": config.partitions,
        "backend": config.backend_kind,
        "database": str(config.db_path),
        "pid": os.getpid(),
    }
    print(json.dumps(ready, ensure_ascii=False), flush=True)

    def _stop(_signum: int, _frame: object) -> None:
        # HTTPServer.shutdown() must not be called from the serve_forever()
        # thread. A short helper thread also makes Ctrl+C reliable on Windows.
        threading.Thread(target=server.shutdown, daemon=True).start()

    previous_handlers: dict[int, object] = {}
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            previous_handlers[sig] = signal.signal(sig, _stop)
        except (ValueError, OSError):
            pass
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        server.server_close()
        coordinator.stop()
        backend.close()
        for sig, handler in previous_handlers.items():
            try:
                signal.signal(sig, handler)
            except (ValueError, OSError):
                pass
    return 0


def _run_worker(args: argparse.Namespace) -> int:
    from .worker import Worker, WorkerOptions, load_fault_config

    config = Config.load(args.config)
    faults = load_fault_config(args.faults, args.member_id) if args.faults else {}
    options = WorkerOptions(
        coordinator_url=args.coordinator,
        member_id=args.member_id,
        session_timeout_ms=args.session_timeout_ms
        or int(faults.get("session_timeout_ms", config.get("worker", "session_timeout_ms", 10000))),
        heartbeat_interval_ms=args.heartbeat_interval_ms
        or int(
            faults.get(
                "heartbeat_interval_ms",
                config.get("worker", "heartbeat_interval_ms", 1000),
            )
        ),
        batch_size=args.batch_size
        or int(faults.get("batch_size", config.get("worker", "batch_size", 250))),
        consume_delay_ms=args.consume_delay_ms
        if args.consume_delay_ms is not None
        else int(
            faults.get(
                "consume_delay_ms", config.get("worker", "consume_delay_ms", 0)
            )
        ),
        stop_after_events=args.stop_after_events
        or int(faults.get("stop_after_events", 0)),
        duration_seconds=args.duration_seconds,
        quiet=args.quiet,
        stop_heartbeat_after_ms=args.stop_heartbeat_after_ms
        if args.stop_heartbeat_after_ms is not None
        else faults.get("stop_heartbeat_after_ms"),
        pause_after_records=args.pause_after_records
        if args.pause_after_records is not None
        else faults.get("pause_after_records"),
        pause_duration_ms=args.pause_duration_ms
        if args.pause_duration_ms is not None
        else int(faults.get("pause_duration_ms", 3000)),
        pause_partition=args.pause_partition
        if args.pause_partition is not None
        else faults.get("pause_partition"),
        stall_after_records=args.stall_after_records
        if args.stall_after_records is not None
        else faults.get("stall_after_records"),
        stall_duration_ms=args.stall_duration_ms
        if args.stall_duration_ms is not None
        else int(faults.get("stall_duration_ms", 3000)),
        processed_log=args.processed_log
        if args.processed_log is not None
        else faults.get("processed_log"),
        no_leave_on_exit=args.no_leave_on_exit
        or bool(faults.get("no_leave_on_exit", False)),
    )
    worker = Worker(options)
    return worker.run()


def _run_generator(args: argparse.Namespace) -> int:
    from .generator import GenerationOptions, generate_events

    config = Config.load(args.config)
    _apply_overrides(config, args)
    backend = None if args.dry_run else create_backend(config)
    try:
        manifest = generate_events(
            GenerationOptions(
                count=args.count,
                seed=args.seed,
                topic=config.topic,
                partitions=config.partitions,
                backend=config.backend_kind,
                bootstrap_servers=str(config.get("kafka", "bootstrap_servers")),
                output_dir=Path(args.output_dir)
                if args.output_dir
                else config.simulator_dir,
                batch_size=args.batch_size,
                jsonl_path=Path(args.jsonl) if args.jsonl else None,
                dry_run=args.dry_run,
            ),
            backend=backend,
        )
    finally:
        if backend is not None:
            backend.close()
    print(json.dumps(manifest, ensure_ascii=False, indent=2), flush=True)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "coordinator":
            return _run_coordinator(args)
        if args.command == "worker":
            return _run_worker(args)
        if args.command == "generate":
            return _run_generator(args)
        parser.error(f"unknown command {args.command}")
    except BackendError as exc:
        print(f"backend error: {exc}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        return 130
    return 1


if __name__ == "__main__":
    raise SystemExit(main())

