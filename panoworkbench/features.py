"""Feature detection and deterministic bidirectional matching."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import cv2
import numpy as np

from .config import FeatureConfig
from .errors import ErrorCode, WorkbenchError


@dataclass(frozen=True)
class FeatureSet:
    image_index: int
    points: np.ndarray
    descriptors: np.ndarray
    scales: np.ndarray
    image_size: tuple[int, int]

    @property
    def count(self) -> int:
        return int(len(self.points))

    def to_summary(self) -> dict[str, Any]:
        return {
            "image_index": self.image_index,
            "keypoints": self.count,
            "width": self.image_size[0],
            "height": self.image_size[1],
            "scale_min": float(np.min(self.scales)) if len(self.scales) else None,
            "scale_max": float(np.max(self.scales)) if len(self.scales) else None,
        }


@dataclass(frozen=True)
class MatchSet:
    points_a: np.ndarray
    points_b: np.ndarray
    source_indices: np.ndarray
    target_indices: np.ndarray
    distances: np.ndarray

    @property
    def count(self) -> int:
        return int(len(self.distances))

    def to_public(self) -> dict[str, Any]:
        return {
            "matches": self.count,
            "mean_distance": float(np.mean(self.distances)) if self.count else None,
        }


def _sort_and_truncate(
    keypoints: tuple[cv2.KeyPoint, ...],
    descriptors: np.ndarray,
    max_features: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    if descriptors is None or len(keypoints) == 0:
        return (
            np.empty((0, 2), dtype=np.float32),
            np.empty((0, 128), dtype=np.float32),
            np.empty((0,), dtype=np.float32),
        )
    order = sorted(
        range(len(keypoints)),
        key=lambda index: (
            -float(keypoints[index].response),
            float(keypoints[index].pt[1]),
            float(keypoints[index].pt[0]),
            float(keypoints[index].angle),
            float(keypoints[index].size),
        ),
    )[:max_features]
    points = np.asarray([keypoints[index].pt for index in order], dtype=np.float32)
    desc = np.asarray(descriptors, dtype=np.float32)[order]
    scales = np.asarray([keypoints[index].size for index in order], dtype=np.float32)
    return points, desc, scales


def detect_features(
    image: np.ndarray,
    image_index: int,
    config: FeatureConfig,
) -> FeatureSet:
    """Detect SIFT features with an explicit, validated budget."""

    config.validate()
    if image.ndim == 2:
        gray = image
    elif image.ndim == 3 and image.shape[2] == 3:
        gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
    elif image.ndim == 3 and image.shape[2] == 4:
        gray = cv2.cvtColor(image, cv2.COLOR_BGRA2GRAY)
    else:
        raise WorkbenchError(
            ErrorCode.META,
            "image must be grayscale, BGR, or BGRA",
            field="channels",
        )
    try:
        sift = cv2.SIFT_create(
            nfeatures=int(config.max_features),
            nOctaveLayers=int(config.octave_layers),
            contrastThreshold=float(config.contrast_threshold),
            edgeThreshold=float(config.edge_threshold),
            sigma=1.6,
        )
    except (AttributeError, cv2.error) as exc:
        raise WorkbenchError(
            ErrorCode.PARA,
            "SIFT is unavailable in this OpenCV build",
            field="features.detector",
        ) from exc
    keypoints, descriptors = sift.detectAndCompute(gray, None)
    points, desc, scales = _sort_and_truncate(
        keypoints or (), descriptors, int(config.max_features)
    )
    if len(points) == 0:
        raise WorkbenchError(
            ErrorCode.MATCH,
            "no features were detected",
            filename=f"image[{image_index}]",
            field="features",
        )
    return FeatureSet(
        image_index=image_index,
        points=points,
        descriptors=desc,
        scales=scales,
        image_size=(int(image.shape[1]), int(image.shape[0])),
    )


def _knn(
    matcher: cv2.BFMatcher,
    query: np.ndarray,
    train: np.ndarray,
    k: int,
) -> list[tuple[cv2.DMatch, ...]]:
    if len(query) == 0 or len(train) == 0:
        return []
    return matcher.knnMatch(query, train, k=k)


def match_features(
    features_a: FeatureSet,
    features_b: FeatureSet,
    config: FeatureConfig,
) -> MatchSet:
    """Ratio-test both directions and retain mutual nearest neighbours."""

    config.validate()
    matcher = cv2.BFMatcher(cv2.NORM_L2, crossCheck=False)
    forward = _knn(matcher, features_a.descriptors, features_b.descriptors, 2)
    reverse = _knn(matcher, features_b.descriptors, features_a.descriptors, 2)
    reverse_first: dict[int, tuple[int, float]] = {}
    reverse_ratio_ok: set[tuple[int, int]] = set()
    for candidates in reverse:
        if len(candidates) < 2:
            continue
        first, second = candidates[0], candidates[1]
        reverse_first[int(first.queryIdx)] = (int(first.trainIdx), float(first.distance))
        if first.distance < config.ratio_test * second.distance:
            reverse_ratio_ok.add((int(first.queryIdx), int(first.trainIdx)))

    good: list[tuple[int, int, float]] = []
    for candidates in forward:
        if len(candidates) < 2:
            continue
        first, second = candidates[0], candidates[1]
        if first.distance >= config.ratio_test * second.distance:
            continue
        query_index = int(first.queryIdx)
        train_index = int(first.trainIdx)
        reverse_pair = reverse_first.get(train_index)
        if reverse_pair is None or reverse_pair[0] != query_index:
            continue
        if (train_index, query_index) not in reverse_ratio_ok:
            continue
        good.append((query_index, train_index, float(first.distance)))

    good.sort(key=lambda item: (item[0], item[1], item[2]))
    if len(good) < config.min_matches:
        raise WorkbenchError(
            ErrorCode.MATCH,
            f"only {len(good)} mutual ratio matches, minimum is {config.min_matches}",
            field="features.min_matches",
            details={"matches": len(good), "minimum": config.min_matches},
        )
    source = np.asarray([item[0] for item in good], dtype=np.int32)
    target = np.asarray([item[1] for item in good], dtype=np.int32)
    return MatchSet(
        points_a=features_a.points[source].astype(np.float64),
        points_b=features_b.points[target].astype(np.float64),
        source_indices=source,
        target_indices=target,
        distances=np.asarray([item[2] for item in good], dtype=np.float64),
    )

