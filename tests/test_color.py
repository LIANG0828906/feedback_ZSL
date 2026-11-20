from __future__ import annotations

from pathlib import Path

import numpy as np

from hdr_studio.color import (
    conversion_matrix,
    convert_frame,
    hlg_inverse_oetf,
    hlg_oetf,
    pq_eotf,
    pq_oetf,
)
from hdr_studio.formats import HDRSequence
from hdr_studio.generator import generate_sample_set
from hdr_studio.model import ColorSpace, ConversionParams, ToneMap


def test_color_matrices_preserve_neutral_and_identity() -> None:
    identity = conversion_matrix(ColorSpace.REC2020, ColorSpace.REC2020)
    np.testing.assert_allclose(identity, np.eye(3), atol=1e-12)
    for source in ColorSpace:
        for target in ColorSpace:
            matrix = conversion_matrix(source, target)
            np.testing.assert_allclose(matrix @ np.ones(3), np.ones(3), atol=1e-12)


def test_pq_and_hlg_transfer_roundtrip() -> None:
    encoded = np.linspace(0.0, 1.0, 1001, dtype=np.float64)
    pq_roundtrip = pq_oetf(pq_eotf(encoded))
    # PQ has a non-zero legal black code; zero-nit luminance maps to ~7.31e-7.
    np.testing.assert_allclose(pq_roundtrip, encoded, atol=1e-6)
    hlg = np.linspace(0.0, 1.0, 1001, dtype=np.float64)
    hlg_roundtrip = hlg_oetf(hlg_inverse_oetf(hlg))
    np.testing.assert_allclose(hlg_roundtrip, hlg, atol=2e-12)


def test_identity_conversion_is_code_exact_for_all_generated_samples(
    tmp_path: Path,
) -> None:
    manifest = generate_sample_set(tmp_path)
    for entry in manifest["samples"]:
        sequence = HDRSequence(tmp_path / "samples" / entry["file"])
        header = sequence.header
        params = ConversionParams(
            target_color_space=header.color_space,
            target_transfer=header.transfer,
            peak_luminance=header.peak_luminance,
            tone_map=ToneMap.NONE,
        )
        for frame_index in range(header.frame_count):
            source = sequence.read_frame(frame_index)
            output, summary = convert_frame(source, header, params, frame_index)
            assert np.array_equal(output, source)
            assert summary["max_code_deviation"] <= 0.5
            assert summary["max_clip_deviation"] == 0.0
            assert summary["out_of_range"] is False


def test_intermediate_summary_and_code_deviation_are_reported(
    tmp_path: Path,
) -> None:
    manifest = generate_sample_set(tmp_path)
    entry = manifest["samples"][0]
    sequence = HDRSequence(tmp_path / "samples" / entry["file"])
    header = sequence.header
    params = ConversionParams(
        target_color_space=ColorSpace.DISPLAY_P3,
        target_transfer=header.transfer,
        peak_luminance=header.peak_luminance,
        tone_map=ToneMap.HABLE,
    )
    _, summary = convert_frame(sequence.read_frame(0), header, params, 0)
    for key in (
        "max_code_deviation",
        "max_clip_deviation",
        "clipped_samples",
        "out_of_range",
        "raw_min_code",
        "raw_max_code",
    ):
        assert key in summary

