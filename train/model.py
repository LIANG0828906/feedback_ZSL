from __future__ import annotations

from typing import Any

import numpy as np

from train.common import parameter_names, parameter_shape


def unpack_parameters(
    vector: np.ndarray, manifest: dict[str, Any]
) -> tuple[np.ndarray, np.ndarray]:
    rows, classes = parameter_shape(manifest)
    flat = np.asarray(vector, dtype=np.float64).reshape(-1)
    weight_count = rows * classes
    expected = weight_count + classes
    if flat.size != expected:
        raise ValueError(f"expected {expected} parameters, got {flat.size}")
    weight = flat[:weight_count].reshape(rows, classes)
    bias = flat[weight_count:]
    return weight, bias


def pack_parameters(weight: np.ndarray, bias: np.ndarray) -> np.ndarray:
    return np.concatenate(
        [np.asarray(weight, dtype=np.float64).reshape(-1), np.asarray(bias, dtype=np.float64)]
    )


def logits(
    features: np.ndarray, vector: np.ndarray, manifest: dict[str, Any]
) -> np.ndarray:
    weight, bias = unpack_parameters(vector, manifest)
    return np.asarray(features, dtype=np.float64) @ weight + bias


def loss_and_gradient(
    features: np.ndarray,
    labels: np.ndarray,
    vector: np.ndarray,
    manifest: dict[str, Any],
) -> tuple[float, np.ndarray]:
    x = np.asarray(features, dtype=np.float64)
    y = np.asarray(labels, dtype=np.int64)
    if x.ndim != 2 or y.ndim != 1 or x.shape[0] != y.shape[0]:
        raise ValueError("features and labels have incompatible shapes")
    weight, _ = unpack_parameters(vector, manifest)
    scores = x @ weight + np.asarray(vector, dtype=np.float64)[-weight.shape[1] :]
    scores -= np.max(scores, axis=1, keepdims=True)
    exponentials = np.exp(scores)
    totals = np.sum(exponentials, axis=1, keepdims=True)
    probabilities = exponentials / totals
    count = x.shape[0]
    selected = probabilities[np.arange(count), y]
    loss = float(-np.mean(np.log(np.maximum(selected, 1e-300))))
    delta = probabilities
    delta[np.arange(count), y] -= 1.0
    delta /= float(count)
    gradient_weight = x.T @ delta
    gradient_bias = np.sum(delta, axis=0)
    return loss, pack_parameters(gradient_weight, gradient_bias)


def loss_only(
    features: np.ndarray,
    labels: np.ndarray,
    vector: np.ndarray,
    manifest: dict[str, Any],
) -> float:
    x = np.asarray(features, dtype=np.float64)
    y = np.asarray(labels, dtype=np.int64)
    weight, bias = unpack_parameters(vector, manifest)
    scores = x @ weight + bias
    scores -= np.max(scores, axis=1, keepdims=True)
    selected = scores[np.arange(x.shape[0]), y]
    log_totals = np.log(np.sum(np.exp(scores), axis=1))
    return float(np.mean(log_totals - selected))


def accuracy(
    features: np.ndarray, labels: np.ndarray, vector: np.ndarray, manifest: dict[str, Any]
) -> float:
    predictions = np.argmax(logits(features, vector, manifest), axis=1)
    return float(np.mean(predictions == np.asarray(labels, dtype=np.int64)))


def gradient_matches_parameter_layout(manifest: dict[str, Any], size: int) -> bool:
    return size == len(parameter_names(manifest))
