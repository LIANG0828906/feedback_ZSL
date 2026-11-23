"""Explainable scale-aware feature-budget and candidate-pair selection."""

from __future__ import annotations

from dataclasses import dataclass
from itertools import combinations
from typing import Any

import cv2
import numpy as np

from .config import FeatureConfig, PipelineConfig, StrategyConfig
from .dataset import Dataset


@dataclass(frozen=True)
class CandidatePair:
    pair_id: str
    image_a: int
    image_b: int

    @property
    def key(self) -> str:
        return self.pair_id


@dataclass(frozen=True)
class StrategyDecision:
    mode: str
    requested_mode: str
    reason: str
    image_count: int
    work_units_mp: float
    high_feature_budget: int
    actual_feature_budget: int
    candidate_pairs: tuple[CandidatePair, ...]
    coarse_screen_used: bool
    coarse_scores_considered: int

    @property
    def candidate_count(self) -> int:
        return len(self.candidate_pairs)

    def plan_fingerprint(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "actual_feature_budget": self.actual_feature_budget,
            "pairs": [[pair.image_a, pair.image_b] for pair in self.candidate_pairs],
            "coarse_screen_used": self.coarse_screen_used,
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "requested_mode": self.requested_mode,
            "mode": self.mode,
            "reason": self.reason,
            "image_count": self.image_count,
            "work_units_mp": self.work_units_mp,
            "high_feature_budget": self.high_feature_budget,
            "actual_feature_budget": self.actual_feature_budget,
            "candidate_pair_count": self.candidate_count,
            "coarse_screen_used": self.coarse_screen_used,
            "coarse_scores_considered": self.coarse_scores_considered,
            "candidate_pairs": [
                [pair.image_a, pair.image_b] for pair in self.candidate_pairs
            ],
        }


def _assign_pair_ids(
    pairs: set[tuple[int, int]],
) -> tuple[CandidatePair, ...]:
    ordered = sorted((min(a, b), max(a, b)) for a, b in pairs)
    return tuple(
        CandidatePair(
            pair_id=f"P{position:04d}_{image_a}_{image_b}",
            image_a=image_a,
            image_b=image_b,
        )
        for position, (image_a, image_b) in enumerate(ordered)
    )


def _orb_fingerprints(
    dataset: Dataset,
) -> dict[int, tuple[np.ndarray, tuple[int, int]]]:
    result: dict[int, tuple[np.ndarray, tuple[int, int]]] = {}
    orb = cv2.ORB_create(nfeatures=180, scaleFactor=1.2, nlevels=4, fastThreshold=8)
    for index in sorted(dataset.images):
        image = dataset.read_image(index, color=True)
        gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
        scale = min(1.0, 180.0 / max(gray.shape[:2]))
        if scale < 1.0:
            gray = cv2.resize(
                gray,
                (max(1, int(gray.shape[1] * scale)), max(1, int(gray.shape[0] * scale))),
                interpolation=cv2.INTER_AREA,
            )
        _, descriptors = orb.detectAndCompute(gray, None)
        if descriptors is None:
            descriptors = np.empty((0, 32), dtype=np.uint8)
        result[index] = (descriptors, (image.shape[1], image.shape[0]))
    return result


def _coarse_overlap_scores(dataset: Dataset) -> dict[tuple[int, int], int]:
    fingerprints = _orb_fingerprints(dataset)
    matcher = cv2.BFMatcher(cv2.NORM_HAMMING, crossCheck=True)
    scores: dict[tuple[int, int], int] = {}
    indices = sorted(dataset.images)
    for image_a, image_b in combinations(indices, 2):
        desc_a = fingerprints[image_a][0]
        desc_b = fingerprints[image_b][0]
        if len(desc_a) == 0 or len(desc_b) == 0:
            scores[(image_a, image_b)] = 0
            continue
        matches = matcher.match(desc_a, desc_b)
        score = sum(1 for match in matches if match.distance <= 72)
        scores[(image_a, image_b)] = int(score)
    return scores


def select_strategy(dataset: Dataset, config: PipelineConfig) -> StrategyDecision:
    config.validate()
    strategy = config.strategy
    indices = sorted(dataset.images)
    image_count = len(indices)
    work_units = image_count * dataset.average_megapixels
    if strategy.mode == "auto":
        if image_count <= strategy.small_max_images or (
            work_units <= strategy.work_unit_threshold_mp
        ):
            mode = "full"
            reason = (
                "small workload: high feature budget and all-pair matching "
                f"(images={image_count} <= {strategy.small_max_images} or "
                f"work={work_units:.2f}MP <= {strategy.work_unit_threshold_mp:.2f}MP)"
            )
        else:
            mode = "restricted"
            reason = (
                "large workload: low feature budget and coarse overlap candidate "
                f"screening (images={image_count} > {strategy.small_max_images} "
                f"and work={work_units:.2f}MP > {strategy.work_unit_threshold_mp:.2f}MP)"
            )
    else:
        mode = strategy.mode
        reason = f"forced {mode} mode"

    if mode == "full":
        selected = set(combinations(indices, 2))
        coarse_used = False
        scores_considered = 0
    else:
        coarse_used = True
        scores = _coarse_overlap_scores(dataset)
        scores_considered = len(scores)
        selected = set()
        if dataset.base_index in indices:
            selected.update(
                (dataset.base_index, index)
                for index in indices
                if index != dataset.base_index
            )
        for image_a, image_b in zip(indices[:-1], indices[1:], strict=True):
            selected.add((image_a, image_b))

        dynamic_cap = min(
            strategy.max_restricted_pairs,
            max(image_count, image_count * 2),
        )
        remaining = sorted(
            ((score, pair) for pair, score in scores.items() if pair not in selected),
            key=lambda item: (-item[0], item[1]),
        )
        added_links: dict[int, int] = {index: 0 for index in indices}
        for score, pair in remaining:
            if len(selected) >= dynamic_cap:
                break
            if score < 4:
                break
            image_a, image_b = pair
            if (
                added_links[image_a] >= strategy.restricted_neighbor_links
                and added_links[image_b] >= strategy.restricted_neighbor_links
            ):
                continue
            selected.add(pair)
            added_links[image_a] += 1
            added_links[image_b] += 1

    feature_budget = (
        strategy.high_feature_budget if mode == "full" else strategy.low_feature_budget
    )
    actual_budget = min(int(feature_budget), int(config.features.max_features))
    actual_budget = max(32, actual_budget)
    pairs = _assign_pair_ids(selected)
    if not pairs:
        raise ValueError("strategy selected no image pairs")
    return StrategyDecision(
        mode=mode,
        requested_mode=strategy.mode,
        reason=reason,
        image_count=image_count,
        work_units_mp=work_units,
        high_feature_budget=actual_budget,
        actual_feature_budget=actual_budget,
        candidate_pairs=pairs,
        coarse_screen_used=coarse_used,
        coarse_scores_considered=scores_considered,
    )


def feature_config_for_plan(
    config: FeatureConfig, decision: StrategyDecision
) -> FeatureConfig:
    return config.with_budget(decision.actual_feature_budget)
