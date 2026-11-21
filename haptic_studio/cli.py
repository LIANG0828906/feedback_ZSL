from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

from .canonical import canonical_json_bytes
from .errors import HapticError
from .models import DeviceConfig, Envelope, WaveSequence
from .samples import (
    DEFAULT_SEED,
    export_csv,
    generate_samples,
    load_config_file,
    load_wave_file,
)
from .session import HapticSession


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_json_bytes(value) + b"\n")


def _load_bundle(
    sample_dir: Path, sample_id: str
) -> tuple[WaveSequence, list[tuple[int, str, DeviceConfig]]]:
    wave = load_wave_file(sample_dir / f"{sample_id}_wave.json")
    configs = load_config_file(sample_dir / f"{sample_id}_devices.json")
    return wave, configs


def command_generate(args: argparse.Namespace) -> int:
    manifest = generate_samples(args.out, seed=args.seed)
    print(json.dumps(manifest, ensure_ascii=False, sort_keys=True))
    return 0


def command_demo(args: argparse.Namespace) -> int:
    output = Path(args.out)
    sample_dir = output / "samples"
    manifest = generate_samples(sample_dir, seed=args.seed)
    sample_id = args.sample
    if not any(item["sample_id"] == sample_id for item in manifest["samples"]):
        raise HapticError(
            "E_CONFIG_INVALID", f"unknown sample id {sample_id}", field="sample"
        )
    sequence, configs = _load_bundle(sample_dir, sample_id)
    runtime = output / "runtime"
    session = HapticSession.create(sequence, configs[0][2], runtime)
    baseline = json.loads(
        (sample_dir / f"{sample_id}_baseline.json").read_text(encoding="utf-8")
    )
    baseline_by_version = {item["version"]: item for item in baseline["versions"]}

    baseline_matches: dict[str, bool] = {}
    baseline_matches["1"] = (
        session.latest_publication.signal_hash == baseline_by_version[1]["signal_hash"]
    )
    publication = session.recompute(
        sequence, new_config=configs[1][2], operation="config_v2"
    )
    baseline_matches["2"] = (
        publication.signal_hash == baseline_by_version[2]["signal_hash"]
    )
    publication = session.recompute(
        sequence, new_config=configs[2][2], operation="config_v3"
    )
    baseline_matches["3"] = (
        publication.signal_hash == baseline_by_version[3]["signal_hash"]
    )

    edited_payload = sequence.to_dict()
    edited_payload["segments"][0]["envelope"] = _next_envelope(
        edited_payload["segments"][0]["envelope"]
    )
    edited = WaveSequence.from_mapping(edited_payload)
    session.recompute(edited, operation="segment_envelope_edit")
    session.rollback_config(2)

    signals_dir = output / "signals"
    verification: list[dict[str, Any]] = []
    for published in session.publication_history():
        export_csv(
            signals_dir / f"version_{published.publication_version:04d}.csv",
            published.sampling.left_array,
            published.sampling.right_array,
            sample_rate=published.config.sample_rate,
            limited=published.sampling.limited_mask,
        )
        verification.append(session.verify_historical(published.publication_version))

    trace_source = session.data_dir / "trace.jsonl"
    trace_target = output / "trace.jsonl"
    trace_target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(trace_source, trace_target)
    summary = {
        "sample_id": sample_id,
        "seed": args.seed,
        "baseline_matches": baseline_matches,
        "history_verification": verification,
        "publications": [
            {
                **item.trace_record(),
                "full_recompute": item.full_recompute,
                "full_reason": item.full_reason,
            }
            for item in session.publication_history()
        ],
        "trace_sha256": hashlib.sha256(trace_target.read_bytes()).hexdigest(),
        "snapshot_sha256": hashlib.sha256(
            (session.data_dir / "session.snapshot.json").read_bytes()
        ).hexdigest(),
    }
    _write_json(output / "summary.json", summary)
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if all(baseline_matches.values()) else 1


def _next_envelope(value: str) -> str:
    values = [item.value for item in Envelope]
    index = values.index(value)
    return values[(index + 1) % len(values)]


def command_recover(args: argparse.Namespace) -> int:
    session = HapticSession.open(args.data_dir)
    payload = {
        "recovery": session.recovery_report.to_dict(),
        "latest_publication": session.latest_publication.publication_version,
        "signal_hash": session.latest_publication.signal_hash,
        "versions": [
            {
                "version": item.publication_version,
                "config_version": item.config_version,
                "signal_hash": item.signal_hash,
                "version_hash": item.version_hash,
            }
            for item in session.publication_history()
        ],
    }
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


def command_crash_recover(args: argparse.Namespace) -> int:
    output = Path(args.out)
    output.mkdir(parents=True, exist_ok=True)
    sample_dir = output / "crash_samples"
    manifest = generate_samples(sample_dir, seed=args.seed)
    sample_id = "dense_9" if args.segments == 9 else "sparse_4"
    sequence, configs = _load_bundle(sample_dir, sample_id)
    runtime = output / "runtime"
    HapticSession.create(sequence, configs[0][2], runtime)

    target_wave = sample_dir / f"{sample_id}_wave.json"
    target_config_path = output / "target_config.json"
    target_config = DeviceConfig.from_mapping(
        {
            "sample_rate": 2000,
            "bit_depth": 12,
            "allocation_ratio": [0.62, 0.38],
            "polarity": [1, -1],
            "limiter": "soft",
        }
    )
    _write_json(target_config_path, target_config.to_dict())
    command = [
        sys.executable,
        "-m",
        "haptic_studio",
        "_crash_child",
        "--data-dir",
        str(runtime),
        "--wave",
        str(target_wave),
        "--config",
        str(target_config_path),
        "--crash-after",
        str(args.crash_after),
    ]
    completed = subprocess.run(command, check=False)
    recovered = HapticSession.open(runtime)
    payload = {
        "sample_id": sample_id,
        "seed": args.seed,
        "child_exit_code": completed.returncode,
        "recovery": recovered.recovery_report.to_dict(),
        "latest_publication": recovered.latest_publication.publication_version,
        "verify_latest": recovered.verify_historical(
            recovered.latest_publication.publication_version
        ),
        "manifest_sample_count": len(manifest["samples"]),
    }
    _write_json(output / "crash_recovery.json", payload)
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


def command_crash_child(args: argparse.Namespace) -> int:
    session = HapticSession.open(args.data_dir)
    sequence = load_wave_file(args.wave)
    config = DeviceConfig.from_mapping(
        json.loads(Path(args.config).read_text(encoding="utf-8"))
    )
    session.recompute(
        sequence,
        new_config=config,
        operation="crash_demo",
        crash_after_segments=args.crash_after,
    )
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .gui import launch

    return launch(
        sample_dir=args.samples,
        runtime_dir=args.runtime,
        seed=args.seed,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="haptic-studio",
        description="Deterministic versioned haptic waveform studio",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="generate deterministic local samples"
    )
    generate.add_argument("--out", default="samples")
    generate.add_argument("--seed", type=int, default=DEFAULT_SEED)
    generate.set_defaults(func=command_generate)

    demo = subparsers.add_parser("demo", help="run the repeatable multi-version demo")
    demo.add_argument("--out", default="artifacts/demo")
    demo.add_argument("--sample", default="dense_9")
    demo.add_argument("--seed", type=int, default=DEFAULT_SEED)
    demo.set_defaults(func=command_demo)

    gui = subparsers.add_parser("gui", help="launch the PySide6 desktop application")
    gui.add_argument("--samples", default="samples")
    gui.add_argument("--runtime", default="artifacts/runtime")
    gui.add_argument("--seed", type=int, default=DEFAULT_SEED)
    gui.set_defaults(func=command_gui)

    recover = subparsers.add_parser(
        "recover", help="open and resume a session snapshot"
    )
    recover.add_argument("--data-dir", required=True)
    recover.set_defaults(func=command_recover)

    crash = subparsers.add_parser(
        "crash-recover", help="demonstrate SIGKILL/checkpoint recovery"
    )
    crash.add_argument("--out", default="artifacts/crash")
    crash.add_argument("--segments", type=int, default=9)
    crash.add_argument("--crash-after", type=int, default=2)
    crash.add_argument("--seed", type=int, default=DEFAULT_SEED)
    crash.set_defaults(func=command_crash_recover)

    child = subparsers.add_parser("_crash_child")
    child.add_argument("--data-dir", required=True)
    child.add_argument("--wave", required=True)
    child.add_argument("--config", required=True)
    child.add_argument("--crash-after", type=int, required=True)
    child.set_defaults(func=command_crash_child)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except HapticError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, sort_keys=True))
        return 2
