from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .models import CandidateSet, Project, RunResult
from .util import human_bp, human_hp, wilson_interval
from .validation import build_effective_values


@dataclass(frozen=True)
class BalanceReport:
    payload: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return self.payload

    def to_markdown(self) -> str:
        payload = self.payload
        budget = payload["budget"]
        lines = [
            f"# 战斗平衡验证报告 · {payload['project']['name']}",
            "",
            f"- 工程: `{payload['project']['id']}`",
            f"- 运行模式: `{payload['run']['mode']}`",
            f"- 工程哈希: `{payload['project']['content_hash'][:16]}…`",
            f"- 算法版本: `{payload['run']['algorithm_version']}`",
            f"- 方案提交: {budget['submitted_candidates']}/{budget['total_candidates']}",
            f"- 组合提交: {budget['submitted_combos']}/{budget['total_combos']}",
            f"- 实际峰值槽位: {budget['actual_peak_slots']}/{budget['configured_parallel_slots']}",
            f"- 每线程缓冲上限: {budget['per_thread_buffer_kb']} KiB",
            "",
            "## 方案总览",
            "",
            "| 方案 | 状态 | 胜率 | 95% CI | 平均伤害区间 (HP) | 采样 | 真值重叠 |",
            "|---|---:|---:|---:|---:|---:|---:|",
        ]
        for candidate in payload["candidates"]:
            lines.append(
                "| {name} | {state} | {win} | {low}–{high} | {dmin}–{dmax} | {samples} | "
                "{overlap}/{scenarios} |".format(
                    name=candidate["name"],
                    state=candidate["state"],
                    win=human_bp(candidate["win_rate_bp"]),
                    low=human_bp(candidate["ci_low_bp"]),
                    high=human_bp(candidate["ci_high_bp"]),
                    dmin=human_hp(candidate["damage_min_total"]),
                    dmax=human_hp(candidate["damage_max_total"]),
                    samples=candidate["samples"],
                    overlap=candidate["truth_overlap_count"],
                    scenarios=len(candidate["scenarios"]),
                )
            )
        lines.extend(
            [
                "",
                "## 解析公式真值",
                "",
                "| 技能 | 攻击配装 | 防御配装 | 期望伤害区间 (HP) | 来源 |",
                "|---|---|---|---:|---|",
            ]
        )
        for case in payload.get("formula_truth", []):
            lines.append(
                f"| {case['skill_id']} | {case['loadout_id']} | {case['defender_loadout_id']} | "
                f"{human_hp(case['expected_min'])}–{human_hp(case['expected_max'])} | "
                f"{case.get('source', '')} |"
            )
        lines.extend(["", "## 预算与取舍", ""])
        for decision in payload["budget"]["decisions"]:
            detail = decision.get("detail", {})
            target = ""
            if detail.get("candidate_id"):
                target = f" · {detail['candidate_id']}/{detail.get('scenario_id', '')}"
            lines.append(
                f"- `{decision['sequence']:03d}` {decision['action']} / {decision['reason']}"
                f" · 组合 {decision['combo_count']} · 剩余采样 {decision['budget_remaining']}{target}"
            )
        lines.extend(
            [
                "",
                "## 阈值诊断",
                "",
                "| 方案 | 场景 | 属性 | 实际 | 阈值 | 结果 |",
                "|---|---|---|---:|---:|---:|",
            ]
        )
        for candidate in payload["candidates"]:
            for scenario in candidate["scenarios"]:
                for threshold in scenario["thresholds"]:
                    lines.append(
                        f"| {candidate['name']} | {scenario['scenario_id']} | {threshold['attribute']} | "
                        f"{threshold['actual']} | {threshold['threshold']} | "
                        f"{'PASS' if threshold['passed'] else 'MISS'} |"
                    )
        lines.extend(
            [
                "",
                "## 结论",
                "",
                f"- 真值内方案/场景组合: {payload['summary']['truth_overlap_combos']}/"
                f"{payload['summary']['total_combos']}",
                f"- 关键阈值命中: {payload['summary']['thresholds_passed']}/"
                f"{payload['summary']['thresholds_total']}",
                f"- 报告状态: `{payload['summary']['status']}`",
                "",
            ]
        )
        return "\n".join(lines)


def build_report(
    project: Project,
    candidate_set: CandidateSet,
    run: RunResult,
) -> BalanceReport:
    truth_win = {
        item["scenario_id"]: item for item in project.truth.get("win_rates", [])
    }
    truth_thresholds: dict[str, list[dict[str, Any]]] = {}
    for item in project.truth.get("thresholds", []):
        truth_thresholds.setdefault(item["scenario_id"], []).append(item)
    results = run.combo_map()
    candidate_payloads: list[dict[str, Any]] = []
    total_overlap = 0
    total_combos = 0
    thresholds_passed = 0
    thresholds_total = 0

    for candidate in candidate_set.candidates:
        effective = build_effective_values(project, candidate)
        candidate_results = [
            result for (candidate_id, _), result in results.items() if candidate_id == candidate.id
        ]
        scenario_payloads: list[dict[str, Any]] = []
        overlap_count = 0
        for scenario in project.scenarios:
            result = results.get((candidate.id, scenario.id))
            if result is None:
                continue
            truth = truth_win.get(scenario.id, {})
            truth_low = int(truth.get("expected_low_bp", 0))
            truth_high = int(truth.get("expected_high_bp", 10_000))
            overlap = not (result.ci_high_bp < truth_low or result.ci_low_bp > truth_high)
            if overlap:
                overlap_count += 1
            total_combos += 1
            if overlap:
                total_overlap += 1
            threshold_rows: list[dict[str, Any]] = []
            primary_id = scenario.primary_loadout_id
            attrs = effective["loadouts"][primary_id]
            for threshold in truth_thresholds.get(scenario.id, []):
                actual = attrs[threshold["attribute"]]
                passed = actual >= int(threshold["threshold"])
                thresholds_total += 1
                if passed:
                    thresholds_passed += 1
                threshold_rows.append(
                    {
                        "id": threshold["id"],
                        "attribute": threshold["attribute"],
                        "actual": actual,
                        "threshold": int(threshold["threshold"]),
                        "passed": passed,
                        "description": threshold.get("description", ""),
                    }
                )
            scenario_payloads.append(
                {
                    "scenario_id": scenario.id,
                    "scenario_name": scenario.name,
                    "mode": scenario.mode,
                    "samples": result.samples,
                    "requested_samples": result.requested_samples,
                    "reduction_reason": result.reduction_reason,
                    "wins_a": result.wins_a,
                    "wins_b": result.wins_b,
                    "draws": result.draws,
                    "win_rate_bp": result.win_rate_bp,
                    "ci_low_bp": result.ci_low_bp,
                    "ci_high_bp": result.ci_high_bp,
                    "ci_width_bp": result.ci_width_bp,
                    "truth_low_bp": truth_low,
                    "truth_high_bp": truth_high,
                    "truth_overlap": overlap,
                    "damage_min_total": result.damage_min_total,
                    "damage_max_total": result.damage_max_total,
                    "damage_mean_total": result.damage_mean_total,
                    "round_curve_a": list(result.round_curve_a),
                    "round_curve_b": list(result.round_curve_b),
                    "thresholds": threshold_rows,
                }
            )
        samples = sum(result.samples for result in candidate_results)
        wins = sum(result.wins_a for result in candidate_results)
        ci_low, ci_high, _ = wilson_interval(wins, samples)
        damage_min = (
            sum(result.damage_min_total for result in candidate_results) // len(candidate_results)
            if candidate_results
            else 0
        )
        damage_max = (
            sum(result.damage_max_total for result in candidate_results) // len(candidate_results)
            if candidate_results
            else 0
        )
        damage_mean = (
            sum(result.damage_mean_total for result in candidate_results) // len(candidate_results)
            if candidate_results
            else 0
        )
        summary = next(
            (item for item in run.candidates if item.candidate_id == candidate.id), None
        )
        candidate_payloads.append(
            {
                "candidate_id": candidate.id,
                "name": candidate.name,
                "priority": candidate.priority,
                "state": summary.state if summary else "UNKNOWN",
                "samples": samples,
                "wins_a": wins,
                "win_rate_bp": (wins * 10_000 + samples // 2) // samples if samples else 0,
                "ci_low_bp": ci_low,
                "ci_high_bp": ci_high,
                "damage_min_total": damage_min,
                "damage_max_total": damage_max,
                "damage_mean_total": damage_mean,
                "truth_overlap_count": overlap_count,
                "scenarios": scenario_payloads,
                "modifications": [mod.to_dict() for mod in candidate.modifications],
            }
        )

    decisions = [decision.to_dict() for decision in run.plan.decisions]
    payload = {
        "report_version": 1,
        "project": {
            "id": project.project_id,
            "name": project.name,
            "content_hash": project.content_hash(),
        },
        "candidate_set_hash": candidate_set.content_hash(),
        "run": {
            "mode": run.mode,
            "interrupted": run.interrupted,
            "algorithm_version": run.state_snapshot.get("algorithm_version", ""),
            "snapshot_context": run.state_snapshot.get("snapshot_context", {}),
            "checkpoint_fingerprint": run.checkpoint_fingerprint,
        },
        "budget": {
            "target_samples": run.plan.target_samples,
            "min_samples": run.plan.min_samples,
            "total_sample_budget": run.plan.total_sample_budget,
            "configured_parallel_slots": run.schedule.configured_parallel_slots,
            "actual_peak_slots": run.schedule.actual_peak_slots,
            "dispatch_rounds": run.schedule.dispatch_rounds,
            "queued_peak": run.schedule.queued_peak,
            "blocked_waves": run.schedule.blocked_waves,
            "restored_combos": run.schedule.restored_combos,
            "simulated_combos": run.schedule.simulated_combos,
            "per_thread_buffer_kb": run.schedule.per_thread_buffer_kb,
            "submitted_candidates": sum(
                1 for item in run.candidates if item.state == "SUBMITTED"
            ),
            "total_candidates": len(run.candidates),
            "submitted_combos": run.schedule.committed_combos,
            "total_combos": run.schedule.total_combos,
            "decisions": decisions,
            "combos": [combo.to_dict() for combo in run.plan.combos],
        },
        "truth": project.truth,
        "formula_truth": project.truth.get("formula_cases", []),
        "candidates": candidate_payloads,
        "summary": {
            "truth_overlap_combos": total_overlap,
            "total_combos": total_combos,
            "thresholds_passed": thresholds_passed,
            "thresholds_total": thresholds_total,
            "status": (
                "COMPLETE"
                if not run.interrupted and run.schedule.committed_combos == run.schedule.total_combos
                else "PARTIAL"
            ),
        },
    }
    return BalanceReport(payload)
