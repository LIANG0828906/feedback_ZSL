from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Iterable

from .audio import inspect_wav, sha256_file
from .engine import SessionEngine
from .errors import AppError
from .generator import generate_sample_file
from .models import ChainConfig, Interval


def _print_json(payload: dict[str, Any], *, quiet: bool = False) -> None:
    if quiet:
        return
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def _load_json_argument(value: str, *, label: str) -> Any:
    candidate = Path(value)
    try:
        if candidate.is_file():
            return json.loads(candidate.read_text(encoding="utf-8"))
    except OSError:
        pass
    try:
        return json.loads(value)
    except json.JSONDecodeError as exc:
        raise AppError("E_INVALID_ARGUMENT", f"{label} 不是合法 JSON") from exc


def _parse_set_values(values: Iterable[str]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for item in values:
        if "=" not in item or "." not in item.split("=", 1)[0]:
            raise AppError(
                "E_INVALID_ARGUMENT", f"--set 需要 section.key=value 格式: {item}"
            )
        dotted, raw_value = item.split("=", 1)
        section, key = dotted.split(".", 1)
        try:
            value = json.loads(raw_value)
        except json.JSONDecodeError:
            value = raw_value
        result.setdefault(section, {})[key] = value
    return result


def _parse_interval_specs(
    specs: Iterable[str], sample_rate: int, total_frames: int
) -> list[Interval]:
    intervals: list[Interval] = []
    for index, spec in enumerate(specs, start=1):
        pieces = spec.split(":")
        if len(pieces) < 2:
            raise AppError("E_INVALID_ARGUMENT", f"区间格式错误: {spec}")
        try:
            start = int(round(float(pieces[0]) * sample_rate))
            end = int(round(float(pieces[1]) * sample_rate))
            gain = float(pieces[2]) if len(pieces) >= 3 and pieces[2] else 0.0
            fade_in = float(pieces[3]) if len(pieces) >= 4 and pieces[3] else 10.0
            fade_out = float(pieces[4]) if len(pieces) >= 5 and pieces[4] else 10.0
        except ValueError as exc:
            raise AppError("E_INVALID_ARGUMENT", f"区间数值错误: {spec}") from exc
        intervals.append(
            Interval(
                id=index,
                start_frame=start,
                end_frame=end,
                gain_db=gain,
                fade_in_ms=fade_in,
                fade_out_ms=fade_out,
            )
        )
    return intervals


class JsonlWriter:
    def __init__(self, path: str | Path | None, *, run_label: str) -> None:
        self.path = Path(path) if path else None
        self.run_label = run_label

    def __call__(self, event: dict[str, Any]) -> None:
        if self.path is None:
            return
        payload = {"run": self.run_label, **event}
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with open(self.path, "a", encoding="utf-8") as handle:
            handle.write(
                json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
                + "\n"
            )
            handle.flush()
            os.fsync(handle.fileno())


def _generate_command(args: argparse.Namespace) -> dict[str, Any]:
    info = generate_sample_file(
        args.out,
        sample_rate=args.rate,
        channels=args.channels,
        seconds=args.seconds,
        seed=args.seed,
    )
    return {
        "ok": True,
        "status": "generated",
        "path": str(Path(args.out).resolve()),
        "sample_rate": info.sample_rate,
        "channels": info.channels,
        "frames": info.frames,
        "sha256": sha256_file(args.out),
        "seed": args.seed,
    }


def _new_command(args: argparse.Namespace) -> dict[str, Any]:
    info = inspect_wav(args.input)
    if args.intervals_file:
        values = _load_json_argument(args.intervals_file, label="--intervals-file")
        if not isinstance(values, list):
            raise AppError("E_INVALID_ARGUMENT", "--intervals-file 必须是区间数组")
        intervals = [
            Interval.from_dict(item, total_frames=info.frames) for item in values
        ]
    else:
        intervals = _parse_interval_specs(args.interval or [], info.sample_rate, info.frames)
    chain = ChainConfig.from_dict(
        _load_json_argument(args.chain_json, label="--chain-json")
        if args.chain_json
        else None
    )
    engine = SessionEngine(args.session)
    report = engine.create(
        input_path=args.input,
        output_path=args.output,
        block_size=args.block_size,
        intervals=intervals,
        chain=chain,
        force=args.force,
    )
    report["status"] = "created"
    return report


def _process_command(args: argparse.Namespace) -> dict[str, Any]:
    trace = JsonlWriter(args.trace, run_label=args.run_label)
    progress_writer = JsonlWriter(args.progress_file, run_label=args.run_label)
    engine = SessionEngine(args.session)
    report = engine.process(
        max_new_blocks=args.max_new_blocks,
        trace=trace,
        progress=progress_writer,
        post_commit_delay=args.post_commit_delay_ms / 1000.0,
    )
    return report


def _recover_command(args: argparse.Namespace) -> dict[str, Any]:
    trace = JsonlWriter(args.trace, run_label=args.run_label)
    return SessionEngine(args.session).recover(trace=trace)


def _status_command(args: argparse.Namespace) -> dict[str, Any]:
    return SessionEngine(args.session).status()


def _update_command(args: argparse.Namespace) -> dict[str, Any]:
    trace = JsonlWriter(args.trace, run_label=args.run_label)
    engine = SessionEngine(args.session)
    if args.intervals_file:
        values = _load_json_argument(args.intervals_file, label="--intervals-file")
        if not isinstance(values, list):
            raise AppError("E_INVALID_ARGUMENT", "--intervals-file 必须是区间数组")
        return engine.update_intervals(values, trace=trace)
    updates: dict[str, Any] = {}
    if args.chain_json:
        raw = _load_json_argument(args.chain_json, label="--chain-json")
        if not isinstance(raw, dict):
            raise AppError("E_INVALID_ARGUMENT", "--chain-json 必须是对象")
        updates = raw
    if args.set_values:
        for section, values in _parse_set_values(args.set_values).items():
            updates.setdefault(section, {}).update(values)
    if not updates:
        raise AppError("E_INVALID_ARGUMENT", "update 需要 --chain-json、--set 或 --intervals-file")
    return engine.update_chain(updates, trace=trace)


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    if not path.is_file():
        return events
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(event, dict):
            events.append(event)
    return events


def _audit_output_regions(
    session_path: Path,
) -> tuple[bool, list[int]]:
    engine = SessionEngine(session_path)
    snapshot = engine.store.load()
    non_zero_uncommitted: list[int] = []
    with open(snapshot.output_path, "rb") as handle:
        for index in range(snapshot.block_count):
            if str(index) in snapshot.completed:
                continue
            start, frames, _ = snapshot.block_bounds(index)
            offset = 44 + start * snapshot.info.channels * 2
            size = frames * snapshot.info.channels * 2
            handle.seek(offset)
            payload = handle.read(size)
            if len(payload) != size or any(payload):
                non_zero_uncommitted.append(index)
    return not non_zero_uncommitted, non_zero_uncommitted


def _run_crash_child(
    session_path: Path,
    trace_path: Path,
    progress_path: Path,
    child_log: Path,
    target_blocks: int,
) -> int:
    command = [
        sys.executable,
        "-m",
        "repair_app",
        "process",
        "--session",
        str(session_path),
        "--post-commit-delay-ms",
        "45",
        "--trace",
        str(trace_path),
        "--progress-file",
        str(progress_path),
        "--run-label",
        "forced-crash",
        "--quiet",
    ]
    project_root = Path(__file__).resolve().parent.parent
    with open(child_log, "wb") as log_handle:
        process = subprocess.Popen(
            command,
            cwd=project_root,
            stdout=log_handle,
            stderr=subprocess.STDOUT,
        )
        deadline = time.monotonic() + 30.0
        observed = 0
        while time.monotonic() < deadline:
            if process.poll() is not None:
                break
            events = _read_jsonl(progress_path)
            observed = len(
                [
                    event
                    for event in events
                    if event.get("event") == "progress"
                    and int(event.get("current_block", -1)) >= 0
                ]
            )
            if observed >= target_blocks:
                process.kill()
                process.wait(timeout=10.0)
                return observed
            time.sleep(0.01)
        if process.poll() is None:
            process.kill()
            process.wait(timeout=10.0)
        raise AppError(
            "E_INTERNAL",
            "强制终止演示未能在子进程完成前捕获目标块",
            observed_progress_events=observed,
            child_log=str(child_log),
        )


def _demo_command(args: argparse.Namespace) -> dict[str, Any]:
    root = Path(args.workdir).resolve()
    root.mkdir(parents=True, exist_ok=True)
    samples_dir = root / "samples"
    samples_dir.mkdir(parents=True, exist_ok=True)

    stereo_path = samples_dir / "deterministic_stereo_48000.wav"
    mono_path = samples_dir / "deterministic_mono_16000.wav"
    generate_sample_file(
        stereo_path, sample_rate=48_000, channels=2, seconds=2.4, seed=args.seed
    )
    generate_sample_file(
        mono_path, sample_rate=16_000, channels=1, seconds=1.5, seed=args.seed
    )

    repeat_path = samples_dir / "deterministic_stereo_48000.repeat.wav"
    generate_sample_file(
        repeat_path, sample_rate=48_000, channels=2, seconds=2.4, seed=args.seed
    )
    generator_byte_equal = stereo_path.read_bytes() == repeat_path.read_bytes()
    repeat_path.unlink()

    session_path = root / "demo.repair.json"
    output_path = root / "demo_repaired.wav"
    trace_path = root / "demo_trace.jsonl"
    progress_path = root / "crash_progress.jsonl"
    child_log = root / "crash_child.log"
    for path in (
        session_path,
        output_path,
        trace_path,
        progress_path,
        child_log,
        Path(str(session_path) + ".blocks"),
        Path(str(session_path) + ".lock"),
    ):
        if path.is_dir():
            import shutil

            shutil.rmtree(path)
        elif path.exists():
            path.unlink()

    rate = 48_000
    initial_intervals = [
        Interval(1, int(0.50 * rate), int(1.25 * rate), gain_db=3.0, fade_in_ms=20, fade_out_ms=20),
        Interval(2, int(1.70 * rate), int(2.30 * rate), gain_db=-2.0, fade_in_ms=15, fade_out_ms=15),
    ]
    engine = SessionEngine(session_path)
    engine.create(
        input_path=stereo_path,
        output_path=output_path,
        block_size=1024,
        intervals=initial_intervals,
        force=True,
    )

    observed_progress = _run_crash_child(
        session_path, trace_path, progress_path, child_log, target_blocks=4
    )
    killed_status = engine.status()
    killed_blocks = int(killed_status["committed_blocks"])
    audit_ok, nonzero_uncommitted = _audit_output_regions(session_path)
    expected_size = 44 + killed_status["frames"] * killed_status["channels"] * 2
    output_size_ok = output_path.stat().st_size == expected_size

    recovered = engine.recover(trace=JsonlWriter(trace_path, run_label="recover"))
    resumed = engine.process(trace=JsonlWriter(trace_path, run_label="resume"))

    moved_intervals = [
        Interval(1, int(0.75 * rate), int(1.45 * rate), gain_db=4.0, fade_in_ms=20, fade_out_ms=20),
        initial_intervals[1],
    ]
    update = engine.update_intervals(
        moved_intervals, trace=JsonlWriter(trace_path, run_label="recompute")
    )
    recomputed = engine.process(trace=JsonlWriter(trace_path, run_label="recompute"))

    replay_session = root / "deterministic_replay.repair.json"
    replay_output = root / "deterministic_replay.wav"
    if replay_session.exists():
        replay_session.unlink()
    if replay_output.exists():
        replay_output.unlink()
    import shutil

    if Path(str(replay_session) + ".blocks").exists():
        shutil.rmtree(Path(str(replay_session) + ".blocks"))
    replay_engine = SessionEngine(replay_session)
    replay_engine.create(
        input_path=stereo_path,
        output_path=replay_output,
        block_size=1024,
        intervals=moved_intervals,
        force=True,
    )
    replay_engine.process(trace=JsonlWriter(trace_path, run_label="deterministic-replay"))

    original_bytes = output_path.read_bytes()
    replay_bytes = replay_output.read_bytes()
    output_byte_equal = original_bytes == replay_bytes
    original_sha = sha256_file(output_path)
    replay_sha = sha256_file(replay_output)
    final_status = engine.status()
    replay_snapshot = replay_engine.store.load()
    original_snapshot = engine.store.load()
    summaries_equal = original_snapshot.completed == replay_snapshot.completed
    trace_events = _read_jsonl(trace_path)

    if not generator_byte_equal:
        raise AppError("E_INTERNAL", "确定性样本生成器字节一致性检查失败")
    if not output_byte_equal or not summaries_equal:
        raise AppError(
            "E_INTERNAL",
            "确定性重放输出不一致",
            original_sha=original_sha,
            replay_sha=replay_sha,
            summaries_equal=summaries_equal,
        )
    if not audit_ok or not output_size_ok:
        raise AppError(
            "E_RECOVERY_INVALID",
            "强制终止后检测到不完整块区域或输出尺寸错误",
            non_zero_uncommitted_blocks=nonzero_uncommitted,
            output_size_ok=output_size_ok,
        )
    if recovered["remaining_blocks"] <= 0:
        raise AppError("E_INTERNAL", "恢复演示未留下未处理块")
    if recomputed["reused_blocks"] <= 0 or not update["dirty_blocks"]:
        raise AppError("E_INTERNAL", "增量重算演示未观察到复用/脏块")

    report = {
        "ok": True,
        "status": "demo_completed",
        "seed": args.seed,
        "workdir": str(root),
        "samples": {
            "stereo": {
                "path": str(stereo_path),
                "sample_rate": 48_000,
                "channels": 2,
                "sha256": sha256_file(stereo_path),
            },
            "mono": {
                "path": str(mono_path),
                "sample_rate": 16_000,
                "channels": 1,
                "sha256": sha256_file(mono_path),
            },
            "generator_byte_equal": generator_byte_equal,
        },
        "forced_crash": {
            "child_progress_events_observed": observed_progress,
            "committed_blocks_at_kill": killed_blocks,
            "output_size_ok": output_size_ok,
            "only_complete_block_regions_written": audit_ok,
            "non_zero_uncommitted_blocks": nonzero_uncommitted,
        },
        "recovery": {
            "recovered_blocks": recovered["recovered_blocks"],
            "remaining_blocks_before_resume": recovered["remaining_blocks"],
            "resumed_status": resumed["status"],
            "resume_reused_blocks": resumed["reused_blocks"],
            "resume_processed_blocks": resumed["processed_blocks"],
        },
        "incremental_recompute": {
            "dirty_blocks": update["dirty_blocks"],
            "reused_blocks": recomputed["reused_blocks"],
            "processed_blocks": recomputed["processed_blocks"],
            "status": recomputed["status"],
        },
        "determinism": {
            "generator_byte_equal": generator_byte_equal,
            "output_byte_equal": output_byte_equal,
            "block_summaries_equal": summaries_equal,
            "output_sha256": original_sha,
            "replay_output_sha256": replay_sha,
        },
        "final": final_status,
        "trace": {
            "path": str(trace_path),
            "event_count": len(trace_events),
        },
    }
    (root / "demo_report.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return report


def _gui_command(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(session_path=args.session)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="voice-repair",
        description="确定性分块语音/音乐修复桌面应用 CLI",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser("generate", help="生成确定性本地 WAV 样本")
    generate_parser.add_argument("--out", required=True)
    generate_parser.add_argument("--rate", type=int, default=48_000)
    generate_parser.add_argument("--channels", type=int, choices=(1, 2), default=2)
    generate_parser.add_argument("--seconds", type=float, default=4.0)
    generate_parser.add_argument("--seed", type=int, default=20_261_120)
    generate_parser.set_defaults(handler=_generate_command)

    new_parser = subparsers.add_parser("new", help="从 WAV 创建修复会话")
    new_parser.add_argument("--input", required=True)
    new_parser.add_argument("--session", required=True)
    new_parser.add_argument("--output", required=True)
    new_parser.add_argument("--block-size", type=int, default=4096)
    new_parser.add_argument("--interval", action="append", help="秒:秒[:增益dB[:淡入ms[:淡出ms]]]")
    new_parser.add_argument("--intervals-file")
    new_parser.add_argument("--chain-json")
    new_parser.add_argument("--force", action="store_true")
    new_parser.set_defaults(handler=_new_command)

    process_parser = subparsers.add_parser("process", help="处理缺失块并输出轨迹")
    process_parser.add_argument("--session", required=True)
    process_parser.add_argument("--max-new-blocks", type=int)
    process_parser.add_argument("--trace")
    process_parser.add_argument("--progress-file")
    process_parser.add_argument("--run-label", default="process")
    process_parser.add_argument("--post-commit-delay-ms", type=float, default=0.0)
    process_parser.add_argument("--quiet", action="store_true")
    process_parser.set_defaults(handler=_process_command)

    recover_parser = subparsers.add_parser("recover", help="校验并重建部分输出")
    recover_parser.add_argument("--session", required=True)
    recover_parser.add_argument("--trace")
    recover_parser.add_argument("--run-label", default="recover")
    recover_parser.set_defaults(handler=_recover_command)

    status_parser = subparsers.add_parser("status", help="读取会话状态")
    status_parser.add_argument("--session", required=True)
    status_parser.set_defaults(handler=_status_command)

    update_parser = subparsers.add_parser("update", help="修改处理参数或区间并使受影响块失效")
    update_parser.add_argument("--session", required=True)
    update_parser.add_argument("--chain-json")
    update_parser.add_argument("--set", dest="set_values", action="append", help="section.key=value")
    update_parser.add_argument("--intervals-file")
    update_parser.add_argument("--trace")
    update_parser.add_argument("--run-label", default="update")
    update_parser.set_defaults(handler=_update_command)

    demo_parser = subparsers.add_parser("demo", help="跑通强制终止、恢复、增量重算与确定性验证")
    demo_parser.add_argument("--workdir", default="artifacts/demo")
    demo_parser.add_argument("--seed", type=int, default=20_261_120)
    demo_parser.set_defaults(handler=_demo_command)

    gui_parser = subparsers.add_parser("gui", help="启动 PySide6 桌面界面")
    gui_parser.add_argument("--session")
    gui_parser.set_defaults(handler=_gui_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = args.handler(args)
        if isinstance(result, int):
            return result
        _print_json(result, quiet=bool(getattr(args, "quiet", False)))
        return 0
    except AppError as exc:
        _print_json(exc.as_dict(), quiet=bool(getattr(args, "quiet", False)))
        if bool(getattr(args, "quiet", False)):
            print(f"{exc.code}: {exc.message}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        _print_json(
            {"ok": False, "code": "E_CANCELLED", "message": "用户中断"},
            quiet=bool(getattr(args, "quiet", False)),
        )
        return 130

