from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path
from typing import Any

from .engine import BuildState, compile_signals
from .errors import HapticError
from .model import WaveSegment, load_project
from .samples import write_manifest, write_samples
from .snapshot import SnapshotManager
from .trace import TraceLogger
from .util import canonical_json_text
from .versioning import Publisher


def _write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def _replace_segment(
    segments: list[WaveSegment],
    index: int,
    **changes: Any,
) -> list[WaveSegment]:
    replacement = WaveSegment(
        **{
            **segments[index].__dict__,
            **changes,
        }
    )
    return [
        replacement if position == index else segment
        for position, segment in enumerate(segments)
    ]


def _signal_fingerprint(state: BuildState) -> dict[str, Any]:
    version = state.version
    return {
        "sample_count": version.sample_count,
        "signal_sha256": hashlib.sha256(version.signal_bytes()).hexdigest(),
        "segment_hashes": [
            {
                "index": record.index,
                "sample_interval": [
                    record.start_sample,
                    record.end_sample,
                ],
                "sample_hash": record.sample_hash,
            }
            for record in version.segment_records
        ],
        "chunk_hashes": [
            {
                "sample_interval": [
                    chunk.start_sample,
                    chunk.end_sample,
                ],
                "signal_hash": chunk.signal_hash,
                "limited_samples": chunk.limited_samples,
                "out_of_range_samples": chunk.out_of_range_samples,
            }
            for chunk in version.chunks
        ],
    }


def run_demo(sample_dir: Path, out_dir: Path) -> dict[str, Any]:
    sample_dir.mkdir(parents=True, exist_ok=True)
    out_dir.mkdir(parents=True, exist_ok=True)
    write_samples(sample_dir)
    write_manifest(sample_dir)

    project = load_project(sample_dir / "mixed_envelopes.json")
    trace = TraceLogger(out_dir / "trace.jsonl", reset=True)

    initial = compile_signals(project.segments, project.config)
    publisher = Publisher()
    publisher.publish(initial.version)
    trace.record_build(initial)
    snapshot_path = SnapshotManager.save(
        out_dir / "snapshot-v1.json",
        publisher,
        initial.state,
    )

    edited = _replace_segment(
        list(project.segments),
        1,
        frequency_hz=95.0,
        amplitude=0.72,
    )
    first_increment = compile_signals(
        edited,
        project.config,
        previous=initial.state,
        modified_indices=[1],
    )
    publisher.publish(
        first_increment.version,
        expected_parent_hash=initial.version.content_hash,
    )
    trace.record_build(first_increment)

    edited = _replace_segment(
        edited,
        4,
        amplitude=0.58,
    )
    final_increment = compile_signals(
        edited,
        project.config,
        previous=first_increment.state,
        modified_indices=[4],
    )
    publisher.publish(
        final_increment.version,
        expected_parent_hash=first_increment.version.content_hash,
    )
    trace.record_build(final_increment)

    full = compile_signals(edited, project.config)
    incremental_fp = _signal_fingerprint(final_increment.state)
    full_fp = _signal_fingerprint(full.state)
    equivalent = (
        final_increment.version.signal_bytes() == full.version.signal_bytes()
        and incremental_fp["segment_hashes"] == full_fp["segment_hashes"]
        and incremental_fp["chunk_hashes"] == full_fp["chunk_hashes"]
    )
    if not equivalent:
        raise HapticError(
            "E_DEMO_ASSERTION",
            "incremental output differs from a clean full rebuild",
        )

    final_snapshot = SnapshotManager.save(
        out_dir / "snapshot.json",
        publisher,
        final_increment.state,
    )
    incomplete_start = final_increment.version.chunks[
        len(final_increment.version.chunks) // 2
    ].start_sample
    partial_snapshot = SnapshotManager.save(
        out_dir / "snapshot-recovery-checkpoint.json",
        publisher,
        final_increment.state,
        incomplete_chunk_starts=[incomplete_start],
    )
    recovered = SnapshotManager.load(final_snapshot)
    partial_recovered = SnapshotManager.load(partial_snapshot)
    recovery_equal = (
        recovered.state.version.signal_bytes() == final_increment.version.signal_bytes()
        and recovered.state.version.content_hash == final_increment.version.content_hash
    )
    partial_recovery_equal = (
        partial_recovered.state.version.signal_bytes()
        == final_increment.version.signal_bytes()
        and partial_recovered.state.version.content_hash
        == final_increment.version.content_hash
    )
    if not recovery_equal or not partial_recovery_equal:
        raise HapticError(
            "E_DEMO_ASSERTION",
            "snapshot recovery did not reproduce the published signal",
        )

    signal_path = out_dir / "signal-latest.bin"
    _write_bytes(signal_path, final_increment.version.signal_bytes())
    recover_head = SnapshotManager.load(snapshot_path).state
    stale_verification = False
    try:
        publisher.get_version(
            initial.version,
            initial.version.content_hash,
        )
    except HapticError as exc:
        stale_verification = exc.code == "E_VERSION_STALE"

    summary = {
        "schema": "haptic-demo-v1",
        "sample": "mixed_envelopes.json",
        "sample_rate": project.config.sample_rate,
        "initial": {
            "version": initial.version.version,
            "content_hash": initial.version.content_hash,
            "sample_count": initial.version.sample_count,
        },
        "operations": [
            {
                "modified_segments": list(first_increment.changed_indices),
                "version": first_increment.version.version,
                "affected_intervals": [
                    list(interval) for interval in first_increment.dirty_ranges
                ],
                "recomputed_samples": first_increment.recomputed_samples,
                "reused_samples": first_increment.reused_samples,
                "content_hash": first_increment.version.content_hash,
            },
            {
                "modified_segments": list(final_increment.changed_indices),
                "version": final_increment.version.version,
                "affected_intervals": [
                    list(interval) for interval in final_increment.dirty_ranges
                ],
                "recomputed_samples": final_increment.recomputed_samples,
                "reused_samples": final_increment.reused_samples,
                "content_hash": final_increment.version.content_hash,
            },
        ],
        "incremental_equals_full": equivalent,
        "recovery_equal": recovery_equal,
        "partial_recovery_equal": partial_recovery_equal,
        "partial_recovery_recomputed_chunks": partial_recovered.recomputed_chunks,
        "partial_recovery_recomputed_intervals": [
            list(interval) for interval in partial_recovered.recomputed_chunk_ranges
        ],
        "version_chain": [entry.to_dict() for entry in publisher.history()],
        "stale_query_rejected": stale_verification,
        "initial_recovery_content_hash": recover_head.version.content_hash,
        "signal_file": signal_path.name,
        "signal_sha256": hashlib.sha256(
            final_increment.version.signal_bytes()
        ).hexdigest(),
        "trace_file": "trace.jsonl",
        "trace_sha256": hashlib.sha256(trace.path.read_bytes()).hexdigest(),
        "snapshots": [
            snapshot_path.name,
            final_snapshot.name,
            partial_snapshot.name,
        ],
    }
    (out_dir / "summary.json").write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return summary


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m haptic.cli",
        description="Deterministic haptic waveform renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate-samples",
        help="write deterministic local JSON samples",
    )
    generate.add_argument(
        "--outdir",
        type=Path,
        default=Path("samples"),
    )

    demo = subparsers.add_parser(
        "demo",
        help="run edits, incremental rebuild, snapshot and recovery",
    )
    demo.add_argument("--samples", type=Path, default=Path("samples"))
    demo.add_argument("--out", type=Path, default=Path("artifacts/demo"))
    demo.add_argument(
        "--quiet",
        action="store_true",
        help="do not print the summary JSON",
    )

    validate = subparsers.add_parser(
        "validate",
        help="validate one local project JSON",
    )
    validate.add_argument("path", type=Path)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "generate-samples":
            paths = write_samples(args.outdir)
            manifest = write_manifest(args.outdir)
            print(
                canonical_json_text(
                    {
                        "samples": [path.name for path in paths],
                        "manifest": str(manifest),
                    }
                )
            )
            return 0
        if args.command == "demo":
            summary = run_demo(args.samples, args.out)
            if not args.quiet:
                print(canonical_json_text(summary))
            return 0
        if args.command == "validate":
            project = load_project(args.path)
            initial = compile_signals(project.segments, project.config)
            print(
                canonical_json_text(
                    {
                        "ok": True,
                        "segments": len(project.segments),
                        "sample_rate": project.config.sample_rate,
                        "sample_count": initial.version.sample_count,
                        "version_hash": initial.version.content_hash,
                    }
                )
            )
            return 0
    except HapticError as exc:
        print(canonical_json_text(exc.to_dict()), file=sys.stderr)
        return 2
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
