"""Command-line entry points, including the headless deterministic demo."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .audio import generate_sample_set
from .config import FingerprintConfig
from .errors import BizError
from .library import Library, TrackSpec
from .migration import MigrationContext
from .records import pcm_compression_metadata
from .util import TraceLogger, canonical_bytes


def _config_from_args(args: argparse.Namespace) -> FingerprintConfig:
    bits = args.quant_bits
    if args.version == 1:
        bits = None
    if args.version == 2 and bits is None:
        bits = 0
    return FingerprintConfig(
        version=args.version,
        time_quant_bits=bits,
        band_edges_hz=tuple(args.band_edges),
        channel_merge=args.channel_merge,
        segment_ms=args.segment_ms,
    )


def _tracks_from_args(args: argparse.Namespace, config: FingerprintConfig) -> list[TrackSpec]:
    tracks: list[TrackSpec] = []
    for value in args.track or []:
        if "=" not in value:
            raise BizError("E_AUDIO_INVALID", "--track must be TRACK_ID=WAV_PATH", {"value": value})
        track_id, path = value.split("=", 1)
        tracks.append(TrackSpec(track_id, Path(path), config))
    if args.samples_dir:
        for path in sorted(Path(args.samples_dir).glob("*.wav")):
            tracks.append(TrackSpec(path.stem if args.track_id_from_stem else path.name, path, config))
    unique: dict[str, TrackSpec] = {}
    for track in tracks:
        if track.track_id in unique:
            raise BizError("E_AUDIO_INVALID", "duplicate track id", {"track_id": track.track_id})
        unique[track.track_id] = track
    if not unique:
        raise BizError("E_AUDIO_INVALID", "no WAV tracks were supplied", {"field": "--track/--samples-dir"})
    return [unique[key] for key in sorted(unique)]


def _emit(value: object) -> None:
    sys.stdout.buffer.write(canonical_bytes(value) + b"\n")
    sys.stdout.buffer.flush()


def _trace(path: str | None) -> TraceLogger | None:
    return TraceLogger(Path(path)) if path else None


def cmd_generate(args: argparse.Namespace) -> int:
    rows = generate_sample_set(Path(args.out), seed=args.seed)
    _emit({"ok": True, "seed": args.seed, "samples": [{key: value for key, value in row.items() if key != "path"} | {"file": Path(str(row["path"])).name} for row in rows]})
    return 0


def cmd_import(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    config.validate()
    library = Library(Path(args.library))
    tracks = [] if args.resume else _tracks_from_args(args, config)
    result = library.import_tracks(
        tracks,
        resume=args.resume,
        crash_after=args.crash_after,
        cancel_after=args.cancel_after,
        trace=_trace(args.trace),
    )
    _emit({"ok": True, **result.as_dict()})
    return 0


def cmd_publish(args: argparse.Namespace) -> int:
    library = Library(Path(args.library))
    result = library.publish(trace=_trace(args.trace))
    _emit({"ok": True, **result})
    return 0


def cmd_query(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    library = Library(Path(args.library))
    result = library.search(
        Path(args.wav),
        config=config,
        track_id=args.query_id,
        top_k=args.top_k,
        expected_version_hash=args.expected_version_hash,
    )
    trace = _trace(args.trace)
    if trace is not None:
        trace.emit(
            "query_result",
            query=args.query_id or Path(args.wav).stem,
            library_version_hash=result["library_version_hash"],
            candidates=[
                {
                    "sample": candidate["sample"],
                    "fingerprint_version": candidate["fingerprint_version"],
                    "offset_ms": candidate["offset_ms"],
                    "confidence": candidate["confidence"],
                    "match_hashes": candidate["match_hashes"][:3],
                }
                for candidate in result["candidates"]
            ],
            alignment_errors=result["alignment_errors"],
            snapshot_released=result["snapshot_released"],
        )
    _emit({"ok": True, **result})
    return 0


def cmd_migrate(args: argparse.Namespace) -> int:
    library = Library(Path(args.library))
    context = MigrationContext(
        channel_merge=args.channel_merge,
        compression=pcm_compression_metadata(args.bit_depth),
    )
    result = library.migrate(context, trace=_trace(args.trace))
    _emit({"ok": True, **result})
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    library = Library(Path(args.library))
    _emit({"ok": True, **library.status()})
    return 0


def cmd_recompute(args: argparse.Namespace) -> int:
    from .fingerprint import compare_extractions

    old = FingerprintConfig(
        version=args.version,
        time_quant_bits=None if args.version == 1 else args.old_quant_bits,
        band_edges_hz=tuple(args.old_band_edges),
        channel_merge=args.old_channel_merge,
        segment_ms=args.segment_ms,
    )
    new = FingerprintConfig(
        version=args.version,
        time_quant_bits=None if args.version == 1 else args.new_quant_bits,
        band_edges_hz=tuple(args.new_band_edges),
        channel_merge=args.new_channel_merge,
        segment_ms=args.segment_ms,
    )
    result = compare_extractions(Path(args.wav), old, new)
    _emit({"ok": True, "report": result})
    return 0


def cmd_demo(args: argparse.Namespace) -> int:
    from .demo import run_demo

    result = run_demo(
        Path(args.work),
        seed=args.seed,
        crash_after=args.crash_after,
        verify_determinism=args.verify_determinism,
    )
    _emit({"ok": True, **result})
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="fpstore")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic PCM16 WAV fixtures")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=20261121)
    generate.set_defaults(func=cmd_generate)

    imp = sub.add_parser("import", help="segment, fingerprint and atomically stage local WAV tracks")
    imp.add_argument("--library", required=True)
    imp.add_argument("--track", action="append", help="TRACK_ID=WAV_PATH; repeatable")
    imp.add_argument("--samples-dir")
    imp.add_argument("--track-id-from-stem", action="store_true")
    imp.add_argument("--resume", action="store_true")
    imp.add_argument("--crash-after", type=int)
    imp.add_argument("--cancel-after", type=int)
    imp.add_argument("--trace")
    _add_config_args(imp)
    imp.set_defaults(func=cmd_import)

    publish = sub.add_parser("publish", help="atomically replace the published library pointer")
    publish.add_argument("--library", required=True)
    publish.add_argument("--trace")
    publish.set_defaults(func=cmd_publish)

    query = sub.add_parser("query", help="query a published snapshot")
    query.add_argument("--library", required=True)
    query.add_argument("--wav", required=True)
    query.add_argument("--query-id")
    query.add_argument("--top-k", type=int, default=5)
    query.add_argument("--expected-version-hash")
    query.add_argument("--trace")
    _add_config_args(query)
    query.set_defaults(func=cmd_query)

    migrate = sub.add_parser("migrate", help="migrate all v1/v2 records to v3 and publish atomically")
    migrate.add_argument("--library", required=True)
    migrate.add_argument("--channel-merge", default="not_recorded")
    migrate.add_argument("--bit-depth", type=int, default=16)
    migrate.add_argument("--trace")
    migrate.set_defaults(func=cmd_migrate)

    status = sub.add_parser("status", help="show published and staging snapshots")
    status.add_argument("--library", required=True)
    status.set_defaults(func=cmd_status)

    recompute = sub.add_parser("recompute-diff", help="recompute one WAV under two configs and report offset deltas")
    recompute.add_argument("--wav", required=True)
    recompute.add_argument("--version", type=int, choices=(1, 2, 3), default=3)
    recompute.add_argument("--old-quant-bits", type=int, default=0)
    recompute.add_argument("--new-quant-bits", type=int, default=5)
    recompute.add_argument("--old-band-edges", type=int, nargs="+", default=[0, 500, 2000, 4000, 8000])
    recompute.add_argument("--new-band-edges", type=int, nargs="+", default=[0, 1000, 3000, 6000])
    recompute.add_argument("--old-channel-merge", default="mean")
    recompute.add_argument("--new-channel-merge", default="left")
    recompute.add_argument("--segment-ms", type=int, default=2000)
    recompute.set_defaults(func=cmd_recompute)

    demo = sub.add_parser("demo", help="run deterministic crash/resume/publish/migration demonstration")
    demo.add_argument("--work", default="artifacts/demo")
    demo.add_argument("--seed", type=int, default=20261121)
    demo.add_argument("--crash-after", type=int, default=3)
    demo.add_argument("--verify-determinism", action="store_true")
    demo.set_defaults(func=cmd_demo)
    return parser


def _add_config_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--version", type=int, choices=(1, 2, 3), default=3)
    parser.add_argument("--quant-bits", type=int, default=0)
    parser.add_argument("--band-edges", type=int, nargs="+", default=[0, 500, 2000, 4000, 8000])
    parser.add_argument("--channel-merge", choices=("mono", "mean", "left", "right", "sum_clip"), default="mean")
    parser.add_argument("--segment-ms", type=int, default=2000)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BizError as exc:
        _emit(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        _emit({"ok": False, "code": "E_CANCELLED", "message": "interrupted by operator"})
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
