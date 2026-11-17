from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import sys
from pathlib import Path
from typing import Any

from .analysis import AlignmentPlan, analyze_alignment
from .constants import (
    E_MANIFEST_FIELD,
    E_RENDER,
    E_RESUME_MISMATCH,
    EXIT_OK,
    EXIT_RUNTIME,
)
from .errors import Diagnostic, DiagnosticError, ResumeMismatch, dump_diagnostics
from .manifest import Manifest, load_manifest, load_sources
from .render import MasterBundle, master_block_bytes, prepare_master
from .reporting import (
    build_master_report,
    build_recovery_report,
    total_blocks,
    write_json_atomic,
)
from .snapshot import (
    build_snapshot,
    commit_snapshot,
    discard_progress,
    read_snapshot,
    verify_resume,
)
from .synth import generate_demo
from .wavio import PCM_HEADER_BYTES, wav_header


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="bangzi-align",
        description="梆子腔五路分轨校验、分段对齐、帮腔分离与母带制作",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="确定性生成五路现场样本")
    generate.add_argument("--out", required=True, help="样本输出目录")
    generate.add_argument("--seed", type=int, default=237, help="整数种子")
    generate.add_argument("--force", action="store_true", help="覆盖已有样本")

    validate = subparsers.add_parser("validate", help="校验清单与五路 WAV")
    validate.add_argument("--manifest", required=True, help="场记清单 JSON")

    render = subparsers.add_parser("render", help="对齐、分离、母带与快照续算")
    render.add_argument("--manifest", required=True, help="场记清单 JSON")
    render.add_argument("--out-dir", help="输出目录，默认为清单旁的 output")
    render.add_argument("--block-samples", type=int, help="覆盖清单中的渲染块长")
    render.add_argument("--report", help="母带报告路径")
    render.add_argument(
        "--force",
        action="store_true",
        help="明确丢弃该输出目录中的旧快照后重新渲染",
    )
    return parser


def _success(payload: dict[str, Any]) -> None:
    json.dump(payload, sys.stdout, ensure_ascii=False, separators=(",", ":"))
    sys.stdout.write("\n")
    sys.stdout.flush()


def _validate_only(manifest_path: str) -> int:
    manifest = load_manifest(manifest_path)
    sources = load_sources(manifest)
    _success(
        {
            "schema": "bangzi-validation/v1",
            "ok": True,
            "manifest_sha256": manifest.raw_sha256,
            "sample_rate": manifest.sample_rate,
            "bit_depth": manifest.bit_depth,
            "channels": [
                {"id": channel.id, "voice": channel.voice}
                for channel in manifest.channels
            ],
            "segments": len(manifest.segments),
            "sources_verified": len(sources.data),
        }
    )
    return EXIT_OK


class _InterruptState:
    def __init__(self) -> None:
        self.requested = False

    def handler(self, signum: int, frame: Any) -> None:
        self.requested = True


_EARLY_INTERRUPT = _InterruptState()


def _initial_output(
    output_path: Path,
    manifest: Manifest,
    total_samples: int,
) -> hashlib._Hash:
    header = wav_header(manifest.sample_rate, manifest.bit_depth, 2, total_samples)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    with output_path.open("wb") as handle:
        handle.write(header)
        handle.truncate(PCM_HEADER_BYTES + total_samples * 6)
        handle.flush()
        os.fsync(handle.fileno())
    return hashlib.sha256(header)


def _prepare(
    manifest_path: str,
) -> tuple[Manifest, SourceSet, AlignmentPlan, MasterBundle]:
    manifest = load_manifest(manifest_path)
    sources = load_sources(manifest)
    plan = analyze_alignment(manifest, sources)
    bundle = prepare_master(manifest, sources, plan)
    return manifest, sources, plan, bundle


def _render_session(
    manifest_path: str,
    out_dir: str | None,
    block_override: int | None,
    report_path_arg: str | None,
    force: bool,
) -> int:
    manifest = load_manifest(manifest_path)
    sources = load_sources(manifest)
    effective_block = (
        int(block_override) if block_override is not None else manifest.block_samples
    )
    if effective_block <= 0:
        raise DiagnosticError(
            [
                Diagnostic(
                    E_MANIFEST_FIELD,
                    "manifest",
                    "block_samples 必须大于 0",
                )
            ]
        )
    root = Path(out_dir) if out_dir else manifest.path.parent / "output"
    root.mkdir(parents=True, exist_ok=True)
    output_path = root / "master.wav"
    snapshot_path = root / "progress.json"
    recovery_path = root / "recovery_report.json"
    report_path = Path(report_path_arg) if report_path_arg else root / "master_report.json"
    output_label = "master.wav"

    if force:
        discard_progress(snapshot_path, output_path)
        for path in (recovery_path, report_path):
            try:
                path.unlink()
            except FileNotFoundError:
                pass

    old_snapshot: dict[str, Any] | None = None
    if snapshot_path.exists():
        old_snapshot = read_snapshot(snapshot_path)
        if (
            old_snapshot is not None
            and block_override is None
            and isinstance(old_snapshot.get("block_samples"), int)
            and not isinstance(old_snapshot.get("block_samples"), bool)
            and old_snapshot["block_samples"] > 0
        ):
            effective_block = old_snapshot["block_samples"]
        early_mismatch: list[str] = []
        if old_snapshot is None:
            early_mismatch.append("snapshot_unreadable")
        else:
            expected_inputs = {
                channel.id: sources.sha256[channel.id] for channel in manifest.channels
            }
            if old_snapshot.get("manifest_sha256") != manifest.raw_sha256:
                early_mismatch.append("manifest_sha256")
            if old_snapshot.get("input_sha256") != expected_inputs:
                early_mismatch.append("input_sha256")
            if old_snapshot.get("block_samples") != effective_block:
                early_mismatch.append("block_samples")
        if early_mismatch:
            discard_progress(snapshot_path, output_path)
            raise ResumeMismatch(
                [
                    Diagnostic(
                        E_RESUME_MISMATCH,
                        "resume",
                        "旧快照与当前清单不一致: " + ",".join(early_mismatch),
                    )
                ]
            )
    else:
        for stale_path in (recovery_path, report_path):
            try:
                stale_path.unlink()
            except FileNotFoundError:
                pass

    plan = analyze_alignment(manifest, sources)
    bundle = prepare_master(manifest, sources, plan)
    blocks_total = total_blocks(plan.master_total_samples, effective_block)
    bytes_per_frame = 6
    restarted = old_snapshot is not None
    interrupt = _InterruptState()
    interrupt.requested = _EARLY_INTERRUPT.requested
    previous_handler = signal.getsignal(signal.SIGINT)
    signal.signal(signal.SIGINT, interrupt.handler)
    try:
        if old_snapshot is not None:
            try:
                blocks_done, digest, complete = verify_resume(
                    old_snapshot,
                    manifest,
                    sources,
                    plan,
                    bundle.finalize,
                    block_samples=effective_block,
                    output_label=output_label,
                    output_path=output_path,
                    bytes_per_frame=bytes_per_frame,
                )
            except ResumeMismatch:
                discard_progress(snapshot_path, output_path)
                raise
            snapshot = old_snapshot
        else:
            digest = _initial_output(
                output_path, manifest, plan.master_total_samples
            )
            blocks_done = 0
            complete = False
            bytes_committed = PCM_HEADER_BYTES
            snapshot = build_snapshot(
                manifest,
                sources,
                plan,
                bundle.finalize,
                block_samples=effective_block,
                blocks_done=0,
                prefix_sha256=digest.hexdigest(),
                output_label=output_label,
                bytes_committed=bytes_committed,
            )
            commit_snapshot(snapshot_path, snapshot)

        if interrupt.requested and not complete:
            recovery = build_recovery_report(
                manifest,
                plan,
                output_label=output_label,
                block_samples=effective_block,
                total_samples=plan.master_total_samples,
                total_blocks=blocks_total,
                blocks_done=blocks_done,
            )
            write_json_atomic(recovery_path, recovery)
            _success(
                {
                    "schema": "bangzi-render-result/v1",
                    "status": "interrupted",
                    "blocks_committed": blocks_done,
                    "total_blocks": blocks_total,
                    "recovery_report": "recovery_report.json",
                }
            )
            return EXIT_OK

        while blocks_done < blocks_total:
            start = blocks_done * effective_block
            end = min(plan.master_total_samples, start + effective_block)
            payload = master_block_bytes(
                bundle.master, manifest.bit_depth, start, end
            )
            with output_path.open("r+b") as handle:
                handle.seek(PCM_HEADER_BYTES + start * bytes_per_frame)
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            digest.update(payload)
            blocks_done += 1
            bytes_committed = (
                PCM_HEADER_BYTES
                + min(blocks_done * effective_block, plan.master_total_samples)
                * bytes_per_frame
            )
            snapshot = build_snapshot(
                manifest,
                sources,
                plan,
                bundle.finalize,
                block_samples=effective_block,
                blocks_done=blocks_done,
                prefix_sha256=digest.hexdigest(),
                output_label=output_label,
                bytes_committed=bytes_committed,
            )
            commit_snapshot(snapshot_path, snapshot)
            if interrupt.requested:
                break

        interrupted = blocks_done < blocks_total
        if interrupted:
            recovery = build_recovery_report(
                manifest,
                plan,
                output_label=output_label,
                block_samples=effective_block,
                total_samples=plan.master_total_samples,
                total_blocks=blocks_total,
                blocks_done=blocks_done,
            )
            write_json_atomic(recovery_path, recovery)
            _success(
                {
                    "schema": "bangzi-render-result/v1",
                    "status": "interrupted",
                    "blocks_committed": blocks_done,
                    "total_blocks": blocks_total,
                    "recovery_report": "recovery_report.json",
                }
            )
            return EXIT_OK

        report = build_master_report(
            manifest,
            plan,
            bundle,
            output_label=output_label,
            blocks_done=blocks_done,
            total_blocks=blocks_total,
            block_samples=effective_block,
            restarted=restarted,
            interrupted=False,
        )
        write_json_atomic(report_path, report)
        try:
            recovery_path.unlink()
        except FileNotFoundError:
            pass
        _success(
            {
                "schema": "bangzi-render-result/v1",
                "status": "complete",
                "blocks_committed": blocks_done,
                "total_blocks": blocks_total,
                "master": output_label,
                "report": report_path.name,
                "measured_lufs": report["loudness"]["measured_lufs"],
                "measured_true_peak_dbtp": report["loudness"][
                    "measured_true_peak_dbtp"
                ],
            }
        )
        return EXIT_OK
    finally:
        signal.signal(signal.SIGINT, previous_handler)


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "generate":
            path = generate_demo(args.out, seed=args.seed, force=args.force)
            _success(
                {
                    "schema": "bangzi-generate-result/v1",
                    "status": "complete",
                    "manifest": path.name,
                }
            )
            return EXIT_OK
        if args.command == "validate":
            return _validate_only(args.manifest)
        if args.command == "render":
            previous_handler = signal.getsignal(signal.SIGINT)
            _EARLY_INTERRUPT.requested = False
            signal.signal(signal.SIGINT, _EARLY_INTERRUPT.handler)
            try:
                return _render_session(
                    args.manifest,
                    args.out_dir,
                    args.block_samples,
                    args.report,
                    args.force,
                )
            finally:
                signal.signal(signal.SIGINT, previous_handler)
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, "cli", "未知命令")]
        )
    except DiagnosticError as exc:
        dump_diagnostics(exc.diagnostics)
        return exc.exit_code
    except Exception as exc:
        dump_diagnostics(
            [
                Diagnostic(
                    E_RENDER,
                    "runtime",
                    f"{type(exc).__name__}: {exc}",
                )
            ]
        )
        return EXIT_RUNTIME


if __name__ == "__main__":
    raise SystemExit(main())

