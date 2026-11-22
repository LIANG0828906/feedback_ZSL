from __future__ import annotations

import argparse
import json
import platform
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy
import PySide6

from .anomalies import default_presets, load_presets
from .engine import EngineConfig, SessionEngine
from .errors import NegotiationError, WorkbenchError
from .loader import load_session
from .loopback import LoopbackRelayServer, LoopbackUdpClient
from .negotiation import negotiate
from .profiles import PROTOCOL_VERSION
from .replaylog import write_run_log
from .runner import execute_paths
from .samplegen import write_sample_set


ROOT = Path(__file__).resolve().parents[1]


def _json_print(payload: Any) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def _write_json(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _codes(result: Any) -> list[str]:
    codes = {code for frame in result.frames for code in frame.codes}
    codes.update(code for item in result.inputs for code in item.codes)
    codes.update(
        event.code
        for event in result.phase_events
        if event.code is not None
    )
    return sorted(codes)


def _path_summary(result: Any) -> dict[str, Any]:
    return {
        "mode": result.mode,
        "final_phase": result.final_phase,
        "frames": len(result.frames),
        "committed": result.stats.frames_committed,
        "missing": result.stats.frames_missing,
        "inputs": len(result.inputs),
        "replayed": result.stats.inputs_replayed,
        "rejected_inputs": result.stats.inputs_rejected,
        "desktop_revision": result.desktop_state["revision"],
        "codes": _codes(result),
    }


def command_generate(args: argparse.Namespace) -> int:
    output = write_sample_set(args.out)
    _json_print({"generated": output, "generator_version": "deterministic-fixture-v1"})
    return 0


def command_validate(args: argparse.Namespace) -> int:
    try:
        sample = load_session(args.sample)
        result = negotiate(sample)
    except WorkbenchError as exc:
        _json_print(exc.to_dict())
        return 2
    _json_print(
        {
            "ok": True,
            "session_id": sample.session_id,
            "content_hash": sample.content_hash,
            "frames": len(sample.frames),
            "inputs": len(sample.inputs),
            "phase_actions": len(sample.phase_script),
            "negotiation": result.to_dict(),
        }
    )
    return 0


def command_relay_test(args: argparse.Namespace) -> int:
    sample = load_session(args.sample)
    with LoopbackRelayServer() as server:
        client = LoopbackUdpClient(port=server.port)
        try:
            ping = client.ping()
            with SessionEngine(
                sample,
                config=EngineConfig(checkpoint_every=0),
                transport=client,
            ) as engine:
                engine.run_until_committed(2)
                stats = engine.stats
                snapshot = engine.ui_snapshot()
            payload = {
                "ok": ping
                and server.stats()["media_received"] > 0
                and server.stats()["input_received"] > 0,
                "bind": f"127.0.0.1:{server.port}",
                "server": server.stats(),
                "client": {
                    "packets_sent": stats.packets_sent,
                    "inputs_replayed": stats.inputs_replayed,
                    "phase": snapshot["phase"],
                },
            }
        finally:
            client.close()
    _json_print(payload)
    return 0 if payload["ok"] else 1


def command_demo(args: argparse.Namespace) -> int:
    sample = load_session(args.sample)
    preset_path = Path(args.sample).parent / "anomalies.json"
    presets = load_presets(preset_path) if preset_path.exists() else default_presets()
    anomaly = presets[args.anomaly]
    data_dir = Path(args.data_dir)
    checkpoint_dir = data_dir / "checkpoints" / "three_path_demo"
    with LoopbackRelayServer() as server:
        def transport_factory() -> LoopbackUdpClient:
            return LoopbackUdpClient(port=server.port)

        config = EngineConfig(
            retry_limit=2,
            checkpoint_every=5,
            max_parallelism=2,
            per_thread_buffer=4,
            anomaly=anomaly,
            enable_checkpoints=True,
        )
        execution = execute_paths(
            sample,
            config,
            checkpoint_dir,
            recovery_after_frames=args.recovery_after,
            transport_factory=transport_factory,
        )
        relay_stats = server.stats()
    if not execution.comparison.ok:
        _json_print(execution.comparison.to_dict())
        return 3

    anomaly_matrix: dict[str, Any] = {}
    expected = {
        "clean": {"E_STATE"},
        "loss": {"E_LOSS", "E_STATE"},
        "reorder": {"E_ORDER", "E_STATE"},
        "capability": {"E_CAP", "E_STATE"},
        "corrupt": {"E_CORRUPT", "E_STATE"},
        "mixed": {
            "E_CAP",
            "E_CORRUPT",
            "E_DUP",
            "E_LOSS",
            "E_ORDER",
            "E_SESSION",
            "E_STATE",
        },
    }
    with LoopbackRelayServer() as anomaly_server:
        for name, preset in presets.items():
            client = LoopbackUdpClient(port=anomaly_server.port)
            try:
                with SessionEngine(
                    sample,
                    config=EngineConfig(
                        retry_limit=2,
                        checkpoint_every=0,
                        anomaly=preset,
                    ),
                    transport=client,
                ) as engine:
                    engine.run_to_end()
                    result = engine.to_run_result(name)
            finally:
                client.close()
            found = set(_codes(result))
            anomaly_matrix[name] = {
                "codes": sorted(found),
                "expected_codes_present": sorted(expected[name].intersection(found)),
                "missing_expected_codes": sorted(expected[name] - found),
                "missing_frames": result.stats.frames_missing,
                "retransmission_requests": result.stats.retransmission_requests,
                "order_events": result.stats.order_events,
                "corrupt_packets": result.stats.packets_corrupt,
                "session_rejected": result.stats.packets_session_rejected,
                "capability_rejections": result.stats.capability_changes_rejected,
                "duplicate_inputs": result.stats.inputs_duplicate,
            }
        anomaly_relay = anomaly_server.stats()
    failed_presets = [
        name
        for name, item in anomaly_matrix.items()
        if item["missing_expected_codes"]
    ]
    document = {
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
        "environment": {
            "python": platform.python_version(),
            "platform": platform.platform(),
            "pyside6": PySide6.__version__,
            "numpy": numpy.__version__,
            "protocol_version": PROTOCOL_VERSION,
        },
        "sample": {
            "path": str(Path(args.sample).resolve()),
            "session_id": sample.session_id,
            "content_hash": sample.content_hash,
            "frames": len(sample.frames),
            "inputs": len(sample.inputs),
        },
        "negotiation": negotiate(sample).to_dict(),
        "paths": {
            "realtime": _path_summary(execution.realtime),
            "recovery": {
                **_path_summary(execution.recovery),
                "checkpoint": {
                    "status": execution.recovery_checkpoint.status,
                    "path": execution.checkpoint_path,
                    "reason": execution.recovery_checkpoint.reason,
                },
            },
            "replay": _path_summary(execution.replay),
        },
        "comparison": execution.comparison.to_dict(),
        "loopback": {
            "three_path_relay": relay_stats,
            "anomaly_relay": anomaly_relay,
        },
        "anomaly_matrix": anomaly_matrix,
        "result": "PASS"
        if not failed_presets and execution.comparison.ok
        else "FAIL",
        "failed_presets": failed_presets,
        "full_results": execution.to_dict(),
    }
    output_path = data_dir / "generated" / "demo_run.json"
    _write_json(output_path, document)
    _write_json(
        data_dir / "generated" / "anomaly_matrix.json",
        anomaly_matrix,
    )
    write_run_log(
        execution.realtime,
        data_dir / "generated" / "realtime_replay.ndjson",
        metadata={"complete": True, "transport": "127.0.0.1-udp-loopback"},
    )
    write_run_log(
        execution.recovery,
        data_dir / "generated" / "recovery_replay.ndjson",
        metadata={
            "complete": True,
            "recovery_after_frames": args.recovery_after,
        },
    )
    _json_print(
        {
            "result": document["result"],
            "output": str(output_path),
            "comparison": execution.comparison.to_dict(),
            "paths": document["paths"],
            "anomaly_codes": {
                name: item["codes"] for name, item in anomaly_matrix.items()
            },
            "failed_presets": failed_presets,
        }
    )
    return 0 if document["result"] == "PASS" else 4


def command_gui(args: argparse.Namespace) -> int:
    from .gui.app import run_gui

    return run_gui(
        args.sample,
        args.data_dir,
        use_loopback=not args.no_loopback,
        auto_start=not args.no_auto_start,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cast-workbench",
        description="Deterministic multi-screen casting protocol workbench",
    )
    subparsers = parser.add_subparsers(dest="command")

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out", default=str(ROOT / "samples"))
    generate.set_defaults(func=command_generate)

    validate = subparsers.add_parser("validate", help="validate a sample")
    validate.add_argument(
        "--sample", default=str(ROOT / "samples" / "sample_session.json")
    )
    validate.set_defaults(func=command_validate)

    relay = subparsers.add_parser("relay-test", help="test local loopback relay")
    relay.add_argument(
        "--sample", default=str(ROOT / "samples" / "sample_session.json")
    )
    relay.set_defaults(func=command_relay_test)

    demo = subparsers.add_parser("demo", help="run deterministic acceptance demo")
    demo.add_argument(
        "--sample", default=str(ROOT / "samples" / "sample_session.json")
    )
    demo.add_argument("--data-dir", default=str(ROOT / "artifacts"))
    demo.add_argument(
        "--anomaly",
        choices=["clean", "loss", "reorder", "capability", "corrupt", "mixed"],
        default="clean",
    )
    demo.add_argument("--recovery-after", type=int, default=18)
    demo.set_defaults(func=command_demo)

    gui = subparsers.add_parser("gui", help="launch PySide6 workbench")
    gui.add_argument(
        "--sample", default=str(ROOT / "samples" / "sample_session.json")
    )
    gui.add_argument("--data-dir", default=str(ROOT / "artifacts"))
    gui.add_argument("--no-loopback", action="store_true")
    gui.add_argument("--no-auto-start", action="store_true")
    gui.set_defaults(func=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not getattr(args, "command", None):
        args = parser.parse_args(["gui"])
    try:
        return int(args.func(args))
    except NegotiationError as exc:
        _json_print(exc.to_dict())
        return 2
    except WorkbenchError as exc:
        _json_print(exc.to_dict())
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
