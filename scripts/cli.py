"""单任务命令行入口。

示例::

    python scripts/cli.py generate --out samples/my --seed 7 --joints 12 --frames 20
    python scripts/cli.py retarget --source samples/my/medium_source_v1.json \
        --target samples/my/medium_target_v3.json --out outputs/one --tolerance 1e-6
    python scripts/cli.py gui --source <src.json> --target <tgt.json>
"""

from __future__ import annotations

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from retarget_pipeline import compat, gui, mapping, retarget, trajectory
from retarget_pipeline.codec import dump_file, load_file
from retarget_pipeline.validate import validate_retarget


def _cmd_generate(args) -> int:
    from retarget_pipeline.samples import generate_sample_set

    sample_set = generate_sample_set(
        args.out, args.name, args.seed, args.joints, args.frames,
        versions=tuple(int(v) for v in args.versions.split(",")))
    print(json.dumps({"name": sample_set.name, "dir": args.out,
                      "versions": sample_set.versions}, ensure_ascii=False, indent=2))
    return 0


def _cmd_retarget(args) -> int:
    source = compat.upgrade_to_v3(load_file(args.source))
    target = load_file(args.target)
    joint_map = mapping.build_mapping(source, target)
    result = retarget.retarget_clip(source, target, joint_map)
    report = validate_retarget(source, target, joint_map, result, args.tolerance)

    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "mapping.json"), "w", encoding="utf-8") as fh:
        json.dump(joint_map.to_dict(), fh, ensure_ascii=False, sort_keys=True, indent=2)
    with open(os.path.join(args.out, "deviation.json"), "w", encoding="utf-8") as fh:
        json.dump(report.to_dict(), fh, ensure_ascii=False, sort_keys=True, indent=2)

    import numpy as np

    from retarget_pipeline.model import AnimDocument

    rotations = np.stack([f.local_rotations for f in result.frames], axis=0)
    translations = np.zeros((result.frame_count, target.joint_count, 3))
    for i, frame in enumerate(result.frames):
        translations[i, 0] = frame.root_translation
    out_doc = AnimDocument(
        version=3, joints=[j.clone() for j in target.joints],
        local_rotations=rotations, local_translations=translations,
        frame_rate=target.frame_rate, metadata={"mapping_hash": result.mapping_hash})
    dump_file(out_doc, os.path.join(args.out, "retarget_v3.json"))

    records = trajectory.frame_records(
        "cli", source.version, joint_map, report, trajectory.document_hash(source))
    log_hash = trajectory.write_trajectory_log(
        os.path.join(args.out, "trajectory.jsonl"), records)
    print(json.dumps({
        "passed": report.passed,
        "max_joint": report.max_joint,
        "max_angle_rad": report.max_angle_rad,
        "skipped_frames": report.skipped_frames,
        "trajectory_hash": log_hash,
        "mapping": {"hits": joint_map.hit_count, "total": joint_map.source_count,
                    "conflicts": len(joint_map.conflicts)},
    }, ensure_ascii=False, indent=2))
    return 0 if report.passed else 2


def _cmd_gui(args) -> int:
    source = compat.upgrade_to_v3(load_file(args.source))
    target = load_file(args.target)
    joint_map = mapping.build_mapping(source, target)
    result = retarget.retarget_clip(source, target, joint_map)
    report = validate_retarget(source, target, joint_map, result, args.tolerance)
    return gui.launch_gui(target, result, joint_map, report,
                          offscreen=args.offscreen)


def main() -> int:
    parser = argparse.ArgumentParser(description="骨骼动画重定向 CLI")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="生成确定性样本")
    gen.add_argument("--out", required=True)
    gen.add_argument("--name", default="custom")
    gen.add_argument("--seed", type=int, default=7)
    gen.add_argument("--joints", type=int, default=12)
    gen.add_argument("--frames", type=int, default=20)
    gen.add_argument("--versions", default="1,2,3")
    gen.set_defaults(func=_cmd_generate)

    rt = sub.add_parser("retarget", help="执行单次重定向并输出报告")
    rt.add_argument("--source", required=True)
    rt.add_argument("--target", required=True)
    rt.add_argument("--out", required=True)
    rt.add_argument("--tolerance", type=float, default=1e-6)
    rt.set_defaults(func=_cmd_retarget)

    gu = sub.add_parser("gui", help="打开预览界面")
    gu.add_argument("--source", required=True)
    gu.add_argument("--target", required=True)
    gu.add_argument("--tolerance", type=float, default=1e-6)
    gu.add_argument("--offscreen", action="store_true")
    gu.set_defaults(func=_cmd_gui)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
