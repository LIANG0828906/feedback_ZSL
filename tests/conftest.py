from __future__ import annotations

import os
from pathlib import Path

import pytest

from repair_app.engine import SessionEngine
from repair_app.generator import generate_sample_file
from repair_app.models import Interval


@pytest.fixture
def stereo_session(tmp_path: Path) -> tuple[SessionEngine, Path, Path]:
    input_path = tmp_path / "input.wav"
    session_path = tmp_path / "session.json"
    output_path = tmp_path / "output.wav"
    generate_sample_file(
        input_path, sample_rate=16_000, channels=2, seconds=1.0, seed=12345
    )
    engine = SessionEngine(session_path)
    engine.create(
        input_path=input_path,
        output_path=output_path,
        block_size=500,
        intervals=[
            Interval(1, 1_000, 3_000, gain_db=3.0, fade_in_ms=5, fade_out_ms=5),
            Interval(2, 5_000, 7_500, gain_db=-2.0, fade_in_ms=5, fade_out_ms=5),
        ],
    )
    return engine, session_path, output_path

