"""命令行入口：生成样本、运行回放、验证基线、恢复与差异自检。"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .config import load_config, write_default_config
from .errors import ReplayError
from .hashing import read_json, write_json_atomic
from .model import MatchDescription
from .samplegen import generate_samples
from .scheduler import ReplayScheduler
from .verifier import verify


def _load_manifest(sample_dir: Path) -> dict:
    return read_json(sample_dir / "manifest.json")


def cmd_init_config(args: argparse.Namespace) -> int:
    path = write_default_config(args.config)
    print(f"配置已写入: {path}")
    return 0


def cmd_generate(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    paths = generate_samples(
        args.samples,
        args.baselines,
        config.task_compute_budget,
        config.task_memory_budget,
    )
    print(f"已生成 {len(paths)} 个样本，目录: {args.samples}")
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    manifest = _load_manifest(Path(args.samples))
    scheduler = ReplayScheduler(config, args.output)
    selected = set(args.match) if args.match else None
    try:
        for entry in manifest["matches"]:
            if selected is not None and entry["match_id"] not in selected:
                continue
            desc = MatchDescription.load(entry["sample"])
            code, _ = scheduler.submit(desc, recover=args.recover)
            if code != "OK":
                print(f"提交被拒 {entry['match_id']}: {code}")
        scheduler.wait_all()
    finally:
        scheduler.shutdown()

    reports = []
    expected_errors = {
        entry["match_id"]: entry.get("expect_error")
        for entry in manifest["matches"]
    }
    for status in scheduler.statuses():
        line = (
            f"{status['match_id']}: {status['status']} "
            f"turn={status['current_turn']}/{status['total_turns']} "
            f"compute={status['compute_total']} memory={status['memory_total']}"
        )
        if status["error_code"]:
            line += f" error={status['error_code']}"
        print(line)
        expected_error = expected_errors.get(status["match_id"])
        if expected_error:
            actual = status["error_code"]
            # 普通预算配置下该样本可正常完成；仅小预算配置要求触发指定错误码。
            ok = actual in (expected_error, None)
            print(
                f"  预算链路: 期望 {expected_error}(小预算配置) 实际 {actual or 'OK'} "
                f"{'符合预期' if ok else '不符合预期'}"
            )
            reports.append(
                {
                    "match_id": status["match_id"],
                    "code": "OK" if ok else "E_UNEXPECTED",
                    "passed": ok,
                    "message": f"期望错误码 {expected_error}",
                }
            )
        elif status["status"] == "COMPLETED":
            baseline = Path(args.baselines) / f"{status['match_id']}.baseline.json"
            report = verify(
                status["match_id"],
                Path(args.output) / status["match_id"],
                baseline,
            )
            reports.append(report.to_dict())
            print(f"  verify: {report.code} {report.message}")
    write_json_atomic(
        Path(args.output) / "verification_reports.json", reports
    )
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    report = verify(
        args.match_id,
        Path(args.output) / args.match_id,
        args.baseline,
    )
    print(json.dumps(report.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if report.passed else 2


def cmd_tamper_demo(args: argparse.Namespace) -> int:
    """制造一处可诊断差异：篡改基线的首个状态哈希，验证差异定位能力。"""
    baseline = read_json(args.baseline)
    original = baseline["turns"][0]["state_hash"]
    baseline["turns"][0]["state_hash"] = "0" * len(original)
    tampered = Path(args.baseline).with_name(
        Path(args.baseline).stem + ".tampered.json"
    )
    write_json_atomic(tampered, baseline)
    report = verify(
        args.match_id, Path(args.output) / args.match_id, tampered
    )
    print(json.dumps(report.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if not report.passed else 3


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="确定性对局回放器")
    parser.add_argument(
        "--config", default="config/replay.json", help="资源池与预算配置文件"
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_init = sub.add_parser("init-config", help="写出默认配置文件")
    p_init.set_defaults(func=cmd_init_config)

    p_gen = sub.add_parser("generate", help="生成确定性样本与基线")
    p_gen.add_argument("--samples", default="samples/matches")
    p_gen.add_argument("--baselines", default="samples/baselines")
    p_gen.set_defaults(func=cmd_generate)

    p_run = sub.add_parser("run", help="并行调度回放并验证")
    p_run.add_argument("--samples", default="samples/matches")
    p_run.add_argument("--baselines", default="samples/baselines")
    p_run.add_argument("--output", default="output/run")
    p_run.add_argument("--match", action="append", help="只运行指定对局，可重复")
    p_run.add_argument("--recover", action="store_true", help="从已提交快照恢复")
    p_run.set_defaults(func=cmd_run)

    p_verify = sub.add_parser("verify", help="对单个任务执行基线比对")
    p_verify.add_argument("match_id")
    p_verify.add_argument("baseline")
    p_verify.add_argument("--output", default="output/run")
    p_verify.set_defaults(func=cmd_verify)

    p_tamper = sub.add_parser("tamper-demo", help="制造基线差异并验证诊断")
    p_tamper.add_argument("match_id")
    p_tamper.add_argument("baseline")
    p_tamper.add_argument("--output", default="output/run")
    p_tamper.set_defaults(func=cmd_tamper_demo)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except ReplayError as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
