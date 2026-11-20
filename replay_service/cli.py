"""脚本化命令行入口，无显示环境下完成全部验收验证。"""

from __future__ import annotations

import argparse
import json
import os
import sys

from .baseline import compare_with_baseline, read_baseline
from .checkpoint import CheckpointStore
from .errors import ReplayError
from .generator import generate_all
from .models import canonical_dumps
from .parser import parse_input_file
from .runner import BatchItem, CancellationToken, replay_batch


def _cmd_generate(args):
    summaries = generate_all(args.out_dir, seed=args.seed)
    print(json.dumps(summaries, ensure_ascii=False, indent=2))


def _load_items(sample_dir: str, work_dir: str):
    items = []
    scenarios = sorted(
        name
        for name in os.listdir(sample_dir)
        if os.path.isdir(os.path.join(sample_dir, name))
    )
    for scenario in scenarios:
        scenario_dir = os.path.join(sample_dir, scenario)
        config, operations = parse_input_file(os.path.join(scenario_dir, "input.json"))
        items.append(
            BatchItem(
                name=scenario,
                config=config,
                operations=operations,
                checkpoint_dir=os.path.join(work_dir, "checkpoints", scenario),
                trajectory_path=os.path.join(
                    work_dir, "trajectories", f"{scenario}.jsonl"
                ),
            )
        )
    return items


def _cmd_replay(args):
    items = _load_items(args.sample_dir, args.work_dir)
    token = CancellationToken()
    outcomes = replay_batch(
        items, parallelism=args.parallelism, resume=not args.fresh, token=token
    )
    report = []
    for outcome in outcomes:
        last = outcome.results[-1]
        report.append(
            {
                "exercise_id": outcome.exercise_id,
                "steps": len(outcome.results),
                "final_cumulative_used": last.cumulative_used,
                "final_cumulative_penalty": last.cumulative_penalty,
                "last_hash": last.step_hash,
                "trajectory": outcome.trajectory_path,
                "checkpoint": outcome.checkpoint_path,
            }
        )
    print(canonical_dumps(report))


def _cmd_verify(args):
    items = _load_items(args.sample_dir, args.work_dir)
    token = CancellationToken()
    outcomes = replay_batch(
        items, parallelism=args.parallelism, resume=False, token=token
    )
    divergences = []
    for item, outcome in zip(items, outcomes):
        baseline = read_baseline(
            os.path.join(args.sample_dir, item.name, "baseline.json")
        )
        divergence = compare_with_baseline(baseline, item.config, outcome.results)
        if divergence is not None:
            divergences.append(divergence.to_dict())
    if divergences:
        print(canonical_dumps({"status": "DIVERGED", "first": divergences[0]}))
        return 2
    print(canonical_dumps({"status": "MATCH", "exercises": len(outcomes)}))
    return 0


def _cmd_resume(args):
    config, operations = parse_input_file(args.input)
    store = CheckpointStore(args.checkpoint_dir)
    if not store.exists():
        raise ReplayError("E_RESUME_INVALID", "不存在可续算的检查点")
    engine = store.resume_engine(config, operations)
    resumed_from = engine.step_index
    engine.run()
    store.write(config, engine.snapshot())
    print(
        canonical_dumps(
            {
                "resumed_from_step": resumed_from,
                "total_steps": engine.config.total_steps,
                "final_cumulative_used": engine.cumulative_used,
                "final_cumulative_penalty": engine.cumulative_penalty,
                "last_hash": engine.prev_hash,
            }
        )
    )


def _cmd_gui(args):
    from .gui import launch

    launch(args.sample_dir, args.work_dir, parallelism=args.parallelism)


def build_parser():
    parser = argparse.ArgumentParser(description="战术演练配额确定性重演服务")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="生成确定性样本与基线")
    gen.add_argument("--out-dir", required=True)
    gen.add_argument("--seed", type=int, default=20261121)
    gen.set_defaults(func=_cmd_generate)

    replay = sub.add_parser("replay", help="并行重演并写检查点/轨迹")
    replay.add_argument("--sample-dir", required=True)
    replay.add_argument("--work-dir", required=True)
    replay.add_argument("--parallelism", type=int, default=1)
    replay.add_argument("--fresh", action="store_true", help="忽略已有检查点")
    replay.set_defaults(func=_cmd_replay)

    verify = sub.add_parser("verify", help="重演结果与基线分叉比对")
    verify.add_argument("--sample-dir", required=True)
    verify.add_argument("--work-dir", required=True)
    verify.add_argument("--parallelism", type=int, default=1)
    verify.set_defaults(func=_cmd_verify)

    resume = sub.add_parser("resume", help="从检查点续算单场演练")
    resume.add_argument("--input", required=True)
    resume.add_argument("--checkpoint-dir", required=True)
    resume.set_defaults(func=_cmd_resume)

    gui = sub.add_parser("gui", help="启动 Tkinter 复核界面")
    gui.add_argument("--sample-dir", required=True)
    gui.add_argument("--work-dir", required=True)
    gui.add_argument("--parallelism", type=int, default=2)
    gui.set_defaults(func=_cmd_gui)
    return parser


def main(argv=None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args) or 0
    except ReplayError as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
