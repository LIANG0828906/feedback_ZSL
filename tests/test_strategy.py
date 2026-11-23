from __future__ import annotations

import dataclasses

from panoworkbench.config import PipelineConfig, StrategyConfig
from panoworkbench.strategy import select_strategy


def test_auto_strategy_prefers_full_for_small_workload(sample_dataset) -> None:
    decision = select_strategy(sample_dataset, PipelineConfig())
    assert decision.mode == "full"
    assert decision.coarse_screen_used is False
    assert decision.actual_feature_budget == 5000
    assert decision.candidate_count == 3


def test_forced_restricted_mode_uses_fewer_candidates_and_budget(
    sample_dataset,
) -> None:
    full = select_strategy(
        sample_dataset,
        PipelineConfig(strategy=StrategyConfig(mode="full")),
    )
    restricted = select_strategy(
        sample_dataset,
        PipelineConfig(strategy=StrategyConfig(mode="restricted")),
    )
    assert restricted.mode == "restricted"
    assert restricted.coarse_screen_used is True
    assert restricted.candidate_count <= full.candidate_count
    assert restricted.actual_feature_budget < full.actual_feature_budget
    assert restricted.to_dict()["reason"]


def test_auto_strategy_can_choose_restricted_without_truth(sample_dataset) -> None:
    config = PipelineConfig(
        strategy=StrategyConfig(
            mode="auto",
            small_max_images=2,
            work_unit_threshold_mp=0.1,
        )
    )
    decision = select_strategy(sample_dataset, config)
    assert decision.mode == "restricted"
    assert decision.coarse_scores_considered == 3
    assert decision.actual_feature_budget == 900


def test_strategy_is_part_of_parameter_version(sample_dataset) -> None:
    full_config = PipelineConfig(strategy=StrategyConfig(mode="full"))
    restricted_config = PipelineConfig(strategy=StrategyConfig(mode="restricted"))
    full = select_strategy(sample_dataset, full_config)
    restricted = select_strategy(sample_dataset, restricted_config)
    full_version = full_config.parameter_version(full.plan_fingerprint())
    restricted_version = restricted_config.parameter_version(
        restricted.plan_fingerprint()
    )
    assert full_version != restricted_version
