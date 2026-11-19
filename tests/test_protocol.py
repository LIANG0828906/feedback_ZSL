"""Feature protocol validation: count, order, type and protocol mismatch."""

import pytest

from gateway.errors import ErrorCode, GatewayError
from gateway.protocol import ProtocolSpec, validate_features


V3 = ProtocolSpec(
    version="rank-features-v1",
    model_version="v3",
    columns=("a", "b", "c"),
    model_file="m.onnx",
)


def test_valid_request_passes():
    validate_features("rank-features-v1", ["a", "b", "c"], [1.0, 2, -3.5], V3)


def test_protocol_mismatch_points_to_protocol_field():
    with pytest.raises(GatewayError) as exc:
        validate_features("rank-features-v2", ["a", "b", "c"], [1.0, 2.0, 3.0], V3)
    assert exc.value.code == ErrorCode.PROTOCOL_MISMATCH
    assert exc.value.field == "protocol_version"


def test_column_count_mismatch():
    with pytest.raises(GatewayError) as exc:
        validate_features("rank-features-v1", ["a", "b"], [1.0, 2.0], V3)
    assert exc.value.code == ErrorCode.FEATURE_COUNT_MISMATCH


def test_feature_count_mismatch():
    with pytest.raises(GatewayError) as exc:
        validate_features("rank-features-v1", ["a", "b", "c"], [1.0, 2.0], V3)
    assert exc.value.code == ErrorCode.FEATURE_COUNT_MISMATCH
    assert exc.value.field == "features"


def test_column_order_mismatch_locates_position():
    with pytest.raises(GatewayError) as exc:
        validate_features(
            "rank-features-v1", ["a", "c", "b"], [1.0, 2.0, 3.0], V3
        )
    assert exc.value.code == ErrorCode.FEATURE_COLUMN_MISMATCH
    assert exc.value.field == "columns[1]"


def test_rejects_bool_and_string_scalars():
    with pytest.raises(GatewayError) as exc:
        validate_features("rank-features-v1", ["a", "b", "c"], [1.0, True, 3.0], V3)
    assert exc.value.code == ErrorCode.FEATURE_TYPE_MISMATCH
    assert exc.value.field == "features[1]"

    with pytest.raises(GatewayError) as exc:
        validate_features("rank-features-v1", ["a", "b", "c"], [1.0, "x", 3.0], V3)
    assert exc.value.code == ErrorCode.FEATURE_TYPE_MISMATCH
