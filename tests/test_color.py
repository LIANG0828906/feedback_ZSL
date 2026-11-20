from __future__ import annotations

import numpy as np
import pytest

from atlas_pipeline.color import build_rgb_to_rgb_matrix
from atlas_pipeline.transfer import decode_transfer, encode_transfer


@pytest.mark.parametrize("transfer", ["linear", "srgb", "pq"])
def test_transfer_roundtrip_is_deterministic(transfer):
    values = np.linspace(0.0, 1.0, 257, dtype=np.float64)
    encoded = encode_transfer(values, transfer, 1000.0)
    decoded = decode_transfer(encoded, transfer, 1000.0)
    assert np.allclose(decoded, values, atol=2e-5, rtol=0.0)
    assert np.array_equal(encoded, encode_transfer(values, transfer, 1000.0))


def test_shared_matrix_has_expected_limits_and_white_adaptation():
    identity = build_rgb_to_rgb_matrix("rec709", "rec709")
    assert np.allclose(identity, np.eye(3), atol=0.0, rtol=0.0)

    rec709_to_2020 = build_rgb_to_rgb_matrix("rec709", "rec2020")
    white = np.ones(3)
    assert np.allclose(rec709_to_2020 @ white, white, atol=1e-12)
    assert not np.allclose(rec709_to_2020, np.eye(3))

    d65_to_d50 = build_rgb_to_rgb_matrix(
        "rec709",
        "rec709",
        source_white="D65",
        target_white="D50",
        adaptation="bradford",
    )
    assert not np.allclose(d65_to_d50, np.eye(3))
    assert np.allclose(d65_to_d50 @ white, white, atol=1e-12)
    assert np.allclose(
        d65_to_d50,
        build_rgb_to_rgb_matrix(
            "rec709",
            "rec709",
            source_white="D65",
            target_white="D50",
            adaptation="bradford",
        ),
        atol=0.0,
        rtol=0.0,
    )


def test_order_independent_tile_results(pipeline):
    chain = pipeline._make_chain(pipeline.current_version_record.config)
    first = chain.transform_tile(
        pipeline.atlas.tile_linear(1),
        pipeline.current_version_record.tile_params[1],
        bit_depth=10,
        tile_index=1,
        reject_exposure_overflow=True,
    )
    _ = chain.transform_tile(
        pipeline.atlas.tile_linear(0),
        pipeline.current_version_record.tile_params[0],
        bit_depth=10,
        tile_index=0,
        reject_exposure_overflow=True,
    )
    second = chain.transform_tile(
        pipeline.atlas.tile_linear(1),
        pipeline.current_version_record.tile_params[1],
        bit_depth=10,
        tile_index=1,
        reject_exposure_overflow=True,
    )
    assert np.array_equal(first.encoded, second.encoded)
    assert first.summary == second.summary
