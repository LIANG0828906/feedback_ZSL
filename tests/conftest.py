from __future__ import annotations

from pathlib import Path

import pytest

from hdr_studio.generator import generate_sample_set
from hdr_studio.model import ColorSpace, ConversionParams, ToneMap, TransferFunction
from hdr_studio.session import ConversionSession


@pytest.fixture()
def sample_workspace(tmp_path: Path) -> tuple[Path, Path]:
    manifest = generate_sample_set(tmp_path)
    sample = tmp_path / "samples" / manifest["samples"][0]["file"]
    return tmp_path, sample


@pytest.fixture()
def valid_params() -> ConversionParams:
    return ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.REINHARD,
    )


@pytest.fixture()
def published_session(
    sample_workspace: tuple[Path, Path], valid_params: ConversionParams
) -> ConversionSession:
    workspace, sample = sample_workspace
    session = ConversionSession(workspace, sample)
    session.submit(valid_params)
    return session

