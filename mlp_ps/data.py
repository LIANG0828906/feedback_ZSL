from __future__ import annotations

from typing import Tuple

import torch

from .config import Config, SHARD_COUNT


def make_dataset(
    seed: int,
    sample_count: int,
    input_dim: int,
    output_dim: int,
    split: str = "train",
) -> Tuple[torch.Tensor, torch.Tensor]:
    split_offset = {"train": 0, "eval": 1_000_003}.get(split)
    if split_offset is None:
        raise ValueError(f"unknown dataset split: {split}")
    generator = torch.Generator(device="cpu")
    generator.manual_seed(seed + split_offset)
    features = torch.randn((sample_count, input_dim), generator=generator, dtype=torch.float32)
    teacher_weight = (
        torch.randn((input_dim, output_dim), generator=generator, dtype=torch.float32) * 0.05
    )
    teacher_bias = torch.randn((output_dim,), generator=generator, dtype=torch.float32) * 0.01
    noise = torch.randn((sample_count, output_dim), generator=generator, dtype=torch.float32) * 0.005
    targets = features @ teacher_weight + teacher_bias + noise
    return features, targets


def train_dataset(config: Config) -> Tuple[torch.Tensor, torch.Tensor]:
    return make_dataset(
        config.seed,
        config.sample_count,
        config.input_dim,
        config.output_dim,
        split="train",
    )


def eval_dataset(config: Config) -> Tuple[torch.Tensor, torch.Tensor]:
    return make_dataset(
        config.seed,
        config.eval_samples,
        config.input_dim,
        config.output_dim,
        split="eval",
    )


def shard_sample_range(config: Config, step: int, shard_id: int) -> Tuple[int, int]:
    if not (0 <= shard_id < SHARD_COUNT):
        raise ValueError("invalid shard id")
    if step < 0 or step >= config.total_steps:
        raise ValueError("step outside training range")
    start = step * config.batch_size + shard_id * config.local_batch_size
    end = start + config.local_batch_size
    if end > config.sample_count:
        raise ValueError("sample range exceeds configured sample budget")
    return start, end


def local_batch(
    features: torch.Tensor,
    targets: torch.Tensor,
    step: int,
    shard_id: int,
    config: Config,
) -> Tuple[torch.Tensor, torch.Tensor, int, int]:
    start, end = shard_sample_range(config, step, shard_id)
    return features[start:end], targets[start:end], start, end
