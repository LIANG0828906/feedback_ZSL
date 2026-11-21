"""Scriptable command-line entry points for reproducible acceptance runs."""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import replace
from pathlib import Path

from .errors import SyncError
from .models import SyncConfig
from .pipeline import run_pipeline
from .service import SyncRequest, SyncService
from .store import ResultStore
from .synthesizer import synthesize_dataset
from .wavio import write_json_atomic


def _parse_int_list(value: str) -> list[int]:
    return [int(item.strip()) for item in value.split(",") if item.strip()]


def _parse_float_list(value: str) -> list[float]:
    return [float(item.strip()) for item in value.split(",") if item.strip()]


def _parse_missing(value: str | None) -> dict[int, list[int]]:
    result: dict[int, list[int]] = {}
    if not value:
        return result
    for chunk in value.split(";"):
        if not chunk.strip():
            continue
        channel_text, indices_text = chunk.split(":", 1)
        result[int(channel_text)] = _parse_int_list(indices_text)
    return result


def _parse_steps(value: str | None) -> dict[int, float]:
    result: dict[int, float] = {}
    if not value:
        return result
    for chunk in value.split(","):
        if not chunk.strip():
            continue
        channel_text, step_text = chunk.split(":", 1)
        result[int(channel_text)] = float(step_text)
    return result


def _config_from_args(args) -> SyncConfig:
    enabled = tuple(_parse_int_list(args.enabled_channels)) if args.enabled_channels else None
    return SyncConfig(
        reference_channel=args.reference,
        enabled_channels=enabled,
        segment_seconds=args.segment_seconds,
        interpolation=args.interpolation,
        strategy=args.strategy,
        duration_strategy_threshold_s=args.duration_threshold,
        channel_strategy_threshold=args.channel_threshold,
        max_lag_seconds=args.max_lag,
        marker_min_confidence=args.marker_min_confidence,
        marker_min_snr=args.marker_min_snr,
        tolerance_samples=args.tolerance,
        linear_min_abs_rate_ppm=args.linear_min_rate_ppm,
        step_min_samples=args.step_min_samples,
        step_bic_gain=args.step_bic_gain,
    )


def _add_sync_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--wav", required=True, help="16-bit PCM WAV path")
    parser.add_argument("--markers", required=True, help="marker description JSON path")
    parser.add_argument("--out", required=True, help="output root")
    parser.add_argument("--segment-seconds", type=float, default=2.0)
    parser.add_argument("--reference", type=int, default=0)
    parser.add_argument("--enabled-channels", default=None, help="comma-separated channel indices")
    parser.add_argument("--interpolation", choices=("linear", "windowed_sinc"), default="linear")
    parser.add_argument("--strategy", choices=("auto", "full", "segmented"), default="auto")
    parser.add_argument("--duration-threshold", type=float, default=600.0)
    parser.add_argument("--channel-threshold", type=int, default=6)
    parser.add_argument("--max-lag", type=float, default=0.20)
    parser.add_argument("--tolerance", type=float, default=1.5)
    parser.add_argument("--marker-min-confidence", type=float, default=0.25)
    parser.add_argument("--marker-min-snr", type=float, default=5.0)
    parser.add_argument("--linear-min-rate-ppm", type=float, default=0.05)
    parser.add_argument("--step-min-samples", type=float, default=1.0)
    parser.add_argument("--step-bic-gain", type=float, default=6.0)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="track-sync")
    subparsers = parser.add_subparsers(dest="command", required=True)

    synth = subparsers.add_parser("synth", help="generate deterministic drift samples")
    synth.add_argument("--out", required=True)
    synth.add_argument("--duration", type=float, default=30.0)
    synth.add_argument("--channels", type=int, default=8)
    synth.add_argument("--sample-rate", type=int, default=48_000)
    synth.add_argument("--marker-interval", type=float, default=0.5)
    synth.add_argument("--seed", type=int, default=20261122)
    synth.add_argument("--noise-rms", type=float, default=0.0035)
    synth.add_argument("--common-rms", type=float, default=0.075)
    synth.add_argument("--drift-ppm", default=None, help="comma-separated per-channel ppm")
    synth.add_argument("--offsets", default=None, help="comma-separated initial offsets")
    synth.add_argument("--missing", default=None, help="channel:index,index;channel:index")
    synth.add_argument("--steps", default=None, help="channel:samples,channel:samples")

    run = subparsers.add_parser("run", help="run one background synchronization job")
    _add_sync_arguments(run)
    run.add_argument("--expected-version", type=int, default=None)
    run.add_argument("--timeout", type=float, default=None)
    run.add_argument("--strict", action="store_true", help="nonzero exit when diagnostics need attention")
    run.add_argument("--print-json", action="store_true")

    compare = subparsers.add_parser(
        "compare",
        help="compare full/segmented strategies and both interpolation modes",
    )
    _add_sync_arguments(compare)
    compare.add_argument("--print-json", action="store_true")

    recover = subparsers.add_parser("recover", help="restore state only from committed disk report")
    recover.add_argument("--out", required=True)
    recover.add_argument("--print-json", action="store_true")

    demo = subparsers.add_parser("demo", help="generate a local sample and run synchronization")
    demo.add_argument("--out", required=True)
    demo.add_argument("--duration", type=float, default=20.0)
    demo.add_argument("--channels", type=int, default=8)
    demo.add_argument("--seed", type=int, default=20261122)
    demo.add_argument("--segment-seconds", type=float, default=2.0)
    demo.add_argument("--interpolation", choices=("linear", "windowed_sinc"), default="linear")
    demo.add_argument("--strategy", choices=("auto", "full", "segmented"), default="auto")
    demo.add_argument("--print-json", action="store_true")
    return parser


def _print_payload(payload: dict | list, print_json: bool) -> None:
    if print_json:
        print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))
    else:
        print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def _summary(snapshot) -> dict:
    payload = snapshot.payload
    return {
        "version": snapshot.version,
        "content_hash": snapshot.content_hash,
        "strategy": payload["strategy"]["selected"],
        "interpolation": payload["compensation"]["mode"],
        "total_samples": payload["compensation"]["total_samples"],
        "tolerance_ok": payload["compensation"]["tolerance_ok"],
        "measured_worst_residual_samples": payload["diagnostics"][
            "measured_worst_residual_samples"
        ],
        "errors": payload["errors"],
        "cache": payload["cache"]["stats"],
        "outputs": payload["outputs"],
    }


def _command_synth(args) -> int:
    manifest = synthesize_dataset(
        args.out,
        duration_seconds=args.duration,
        channels=args.channels,
        sample_rate=args.sample_rate,
        marker_interval_seconds=args.marker_interval,
        seed=args.seed,
        noise_rms=args.noise_rms,
        common_rms=args.common_rms,
        drift_ppm=_parse_float_list(args.drift_ppm) if args.drift_ppm else None,
        initial_offsets=_parse_float_list(args.offsets) if args.offsets else None,
        missing_markers=_parse_missing(args.missing),
        step_samples=_parse_steps(args.steps),
    )
    print(json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


def _command_run(args) -> int:
    config = _config_from_args(args)
    service = SyncService(args.out)
    try:
        state = service.submit(
            SyncRequest(
                wav_path=args.wav,
                markers_path=args.markers,
                config=config,
                expected_version=args.expected_version,
            )
        )
        state = service.wait(state.job_id, timeout=args.timeout)
        if state.status != "succeeded":
            print(json.dumps(state.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
            return 3 if state.status == "cancelled" else 2
        snapshot = service.get_result(
            version=state.result_version,
            content_hash=state.result_hash,
        )
        summary = _summary(snapshot)
        _print_payload(summary, args.print_json)
        if args.strict and summary["errors"]:
            return 4
        return 0
    except SyncError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
        return 2


def _run_direct(
    args,
    output_root: Path,
    config: SyncConfig,
    run_id: str,
    previous_payload: dict | None,
):
    service = SyncService(output_root)
    report = run_pipeline(
        args.wav,
        args.markers,
        output_root,
        config,
        previous_payload=previous_payload,
        run_id=run_id,
    )
    return service.store.publish(report)


def _command_compare(args) -> int:
    root = Path(args.out).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    base_config = _config_from_args(args)
    service = SyncService(root)
    previous_payload = service.store.current.payload if service.store.current else None

    linear = _run_direct(
        args,
        root,
        replace(base_config, strategy="segmented", interpolation="linear"),
        "compare-segmented-linear",
        previous_payload,
    )
    sinc = _run_direct(
        args,
        root,
        replace(base_config, strategy="segmented", interpolation="windowed_sinc"),
        "compare-segmented-sinc",
        linear.payload,
    )
    full = _run_direct(
        args,
        root,
        replace(base_config, strategy="full", interpolation="linear"),
        "compare-full-linear",
        sinc.payload,
    )

    def view(snapshot):
        payload = snapshot.payload
        return {
            "version": snapshot.version,
            "content_hash": snapshot.content_hash,
            "strategy": payload["strategy"]["selected"],
            "interpolation": payload["compensation"]["mode"],
            "segment_count": payload["strategy"]["segment_count"],
            "total_samples": payload["compensation"]["total_samples"],
            "sample_count_preserved": (
                payload["compensation"]["total_samples"]
                == payload["input"]["frames"]
            ),
            "marker_count": payload["marker_detection"]["marker_description"][
                "marker_count"
            ],
            "measured_worst_residual_samples": payload["diagnostics"][
                "measured_worst_residual_samples"
            ],
            "expected_residual_bound_samples": payload["strategy"].get(
                "estimated_residual_bound_samples"
            ),
            "tolerance_ok": payload["compensation"]["tolerance_ok"],
            "elapsed_seconds": payload["strategy"]["elapsed_seconds"],
            "per_channel_after_max": {
                str(item["channel"]): max(
                    (value for value in item["after_max"] if value is not None),
                    default=None,
                )
                for item in payload["compensation"]["channels"]
            },
        }

    comparison = {
        "same_source_sha256": linear.payload["input"]["sha256"],
        "strategies": {
            "segmented": view(linear),
            "full": view(full),
        },
        "interpolation": {
            "linear": view(linear),
            "windowed_sinc": view(sinc),
            "comparison": sinc.payload["compensation"]["interpolation_comparison"],
        },
        "invariants": {
            "same_total_samples": len(
                {
                    linear.payload["compensation"]["total_samples"],
                    full.payload["compensation"]["total_samples"],
                    sinc.payload["compensation"]["total_samples"],
                }
            )
            == 1,
            "same_marker_detection": len(
                {
                    linear.payload["marker_detection"]["marker_description"]["signature"],
                    full.payload["marker_detection"]["marker_description"]["signature"],
                    sinc.payload["marker_detection"]["marker_description"]["signature"],
                }
            )
            == 1,
            "same_channel_count": len(
                {
                    linear.payload["compensation"]["channel_count"],
                    full.payload["compensation"]["channel_count"],
                    sinc.payload["compensation"]["channel_count"],
                }
            )
            == 1,
        },
    }
    comparison_path = root / "comparison.json"
    write_json_atomic(comparison_path, comparison)
    comparison["comparison_path"] = str(comparison_path)
    _print_payload(comparison, args.print_json)
    return 0


def _command_recover(args) -> int:
    service = SyncService(args.out)
    snapshot = service.recover_from_disk()
    if snapshot is None:
        print(json.dumps({"status": "no_committed_report"}, ensure_ascii=False))
        return 1
    payload = {
        "status": "recovered",
        "version": snapshot.version,
        "content_hash": snapshot.content_hash,
        "run_id": snapshot.payload.get("run_id"),
        "outputs": snapshot.payload.get("outputs", {}),
    }
    _print_payload(payload, args.print_json)
    return 0


def _command_demo(args) -> int:
    root = Path(args.out).expanduser().resolve()
    sample_dir = root / "sample"
    manifest = synthesize_dataset(
        sample_dir,
        duration_seconds=args.duration,
        channels=args.channels,
        seed=args.seed,
    )
    config = SyncConfig(
        segment_seconds=args.segment_seconds,
        interpolation=args.interpolation,
        strategy=args.strategy,
    )
    service = SyncService(root)
    state = service.submit(
        SyncRequest(manifest["wav"], manifest["markers"], config)
    )
    state = service.wait(state.job_id)
    if state.status != "succeeded":
        print(json.dumps(state.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
        return 2
    snapshot = service.get_result(state.result_version, state.result_hash)
    summary = _summary(snapshot)
    summary["sample"] = manifest
    _print_payload(summary, args.print_json)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.command == "synth":
        return _command_synth(args)
    if args.command == "run":
        return _command_run(args)
    if args.command == "compare":
        return _command_compare(args)
    if args.command == "recover":
        return _command_recover(args)
    if args.command == "demo":
        return _command_demo(args)
    parser.error("unknown command")
    return 2


if __name__ == "__main__":
    sys.exit(main())
