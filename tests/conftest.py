from __future__ import annotations

import os
import shutil
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from sciseq.config import BudgetConfig, DenoiseConfig, PipelineConfig, RegistrationConfig
from sciseq.generator import GroupSpec, generate_group
from sciseq.loader import load_sequence


@pytest.fixture(scope="session")
def tiny_sequence(tmp_path_factory: pytest.TempPathFactory):
    root = tmp_path_factory.mktemp("samples")
    spec = GroupSpec("tiny", 128, 160, 8, 7331, 3.0, 9.0, 2.8)
    generate_group(root, spec)
    return load_sequence(root / "tiny")


@pytest.fixture(scope="session")
def tiny_config() -> PipelineConfig:
    return PipelineConfig(
        reference_frame=0,
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=6.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=16, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=120.0,
            memory_budget_mb=512.0,
            max_workers=3,
            frames_per_worker=1,
            preview_denoise_fraction=0.6,
            preview_mean_abs_limit=35.0,
        ),
    )


@pytest.fixture()
def sequence_copy(tiny_sequence, tmp_path: Path):
    target = tmp_path / "tiny"
    shutil.copytree(tiny_sequence.root, target)
    return target

