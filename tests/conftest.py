import os
import sys
from pathlib import Path

import numpy as np
import pytest


os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sciworkbench.factors import save_factors
from sciworkbench.model import load_sequence, write_sequence
from sciworkbench.sample import SampleProfile, generate_frames_and_factors


@pytest.fixture
def small_profile():
    return SampleProfile(
        name="test_64x48x3",
        width=64,
        height=48,
        frames=3,
        bad_density=0.004,
        noise_sigma=1.2,
        seed=4242,
        block_size=16,
        frac_bits=10,
    )


@pytest.fixture
def prepared_sample(tmp_path, small_profile):
    frames, factors = generate_frames_and_factors(small_profile)
    sequence_path = tmp_path / "sequence.simg"
    calibration_path = tmp_path / "factors.cal"
    write_sequence(sequence_path, frames)
    sequence = load_sequence(sequence_path, block_size=small_profile.block_size)
    factors.validate(sequence.structure)
    save_factors(calibration_path, factors, sequence.structure)
    return {
        "root": tmp_path,
        "sequence_path": sequence_path,
        "calibration_path": calibration_path,
        "sequence": sequence,
        "factors": factors,
        "profile": small_profile,
    }
