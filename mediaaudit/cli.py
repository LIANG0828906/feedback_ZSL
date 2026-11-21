"""无显示环境下的脚本化入口：生成样本并执行完整审看流程。

用法::

    python -m mediaaudit.cli --work-dir ./work --sample stereo_48k

流程覆盖：参数多版本提交、增量重算、跨档校验、回退重算与哈希比对、
陈旧缓存拒绝、断点恢复（重新载入引擎）以及轨迹日志落盘。
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from .engine import AuditEngine
from .errors import AuditError
from .samplegen import SAMPLE_SPECS, TIER_SEQUENCES, generate_samples
from .span import assert_spans_consistent
from .timebase import load_source
from .util import canonical_json, content_hash


def run(work_dir: str | Path, sample_name: str) -> dict:
    work = Path(work_dir)
    work.mkdir(parents=True, exist_ok=True)
    baseline = generate_samples(work)
    spec = next(item for item in SAMPLE_SPECS if item.name == sample_name)
    source = load_source(
        work / "sources" / sample_name / "source.wav",
        work / "sources" / sample_name / "frames.json",
    )

    engine = AuditEngine(source, work / "audit")
    summary: dict = {"sample": sample_name, "steps": []}

    # 1) 各档位按固定序列提交参数并重算。
    for tier, seq in TIER_SEQUENCES.items():
        for params in seq:
            record = engine.submit_params(tier, params)
            outcome = engine.recompute(tier)
            summary["steps"].append(
                {
                    "action": "recompute",
                    "tier": tier,
                    "version": record.version,
                    "reuse": len(outcome.reused),
                    "recompute": len(outcome.recomputed),
                }
            )

    # 2) 跨档一致性校验（量化网格不同，预期 E_SPAN_MISMATCH）。
    span_result = {"consistent": False}
    try:
        assert_spans_consistent(
            source,
            "tier_low",
            engine.params.latest("tier_low").version,
            engine.params.latest("tier_low").params,
            "tier_high",
            engine.params.latest("tier_high").version,
            engine.params.latest("tier_high").params,
        )
        span_result["consistent"] = True
    except AuditError as exc:
        span_result["code"] = exc.code.value
        span_result["first_mismatch"] = exc.details
    summary["span_check"] = span_result

    # 3) 回退 tier_low 到 v1，生成新版本并验证哈希与 v1 逐片一致。
    rollback_record = engine.rollback_params("tier_low", 1)
    rollback_outcome = engine.recompute("tier_low")
    v1_hashes = [
        a.content_hash_value
        for a in _load_version_artifacts(engine, "tier_low", 1)
    ]
    rb_hashes = [a.content_hash_value for a in rollback_outcome.artifacts]
    summary["rollback"] = {
        "new_version": rollback_record.version,
        "origin": rollback_record.origin,
        "hash_match_v1": v1_hashes == rb_hashes,
        "stale_rejections": rollback_outcome.stale_rejections,
    }

    # 4) 强制终止恢复：新建引擎实例，从清单恢复且不重复写入切片。
    recovered = AuditEngine(source, work / "audit")
    recovered_rows = recovered.store.load_manifest()
    summary["recovery"] = {
        "manifest_rows": len(recovered_rows),
        "committed": {
            tier: dict(versions) for tier, versions in recovered.committed.items()
        },
        "graph_version": recovered.graph.graph_version,
    }

    summary["trajectory"] = str(engine.trajectory_path)
    (work / "run_summary.json").write_bytes(canonical_json(summary))
    return summary


def _load_version_artifacts(
    engine: AuditEngine, tier: str, version: int
) -> list:
    count = engine.committed[tier][version]
    hashes = []
    for index in range(count):
        blob = engine.store.read_slice_bytes(tier, version, index)
        payload = json.loads(blob)
        hashes.append(content_hash(payload))
    return [
        type(
            "HashView",
            (),
            {"content_hash_value": item},
        )
        for item in hashes
    ]


def main() -> None:
    parser = argparse.ArgumentParser(description="赛事素材审看确定性入口")
    parser.add_argument("--work-dir", default="./work")
    parser.add_argument(
        "--sample",
        default=SAMPLE_SPECS[0].name,
        choices=[spec.name for spec in SAMPLE_SPECS],
    )
    args = parser.parse_args()
    summary = run(args.work_dir, args.sample)
    print(json.dumps(summary, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
