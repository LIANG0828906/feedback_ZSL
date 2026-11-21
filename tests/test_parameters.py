"""要求 2：档位参数版本模型。"""

from __future__ import annotations

from pathlib import Path

import pytest

from mediaaudit.errors import ErrorCode
from mediaaudit.parameters import ParameterStore, TierParams


def _params(rate: int = 16000, gain: int = 256, quantum: int = 1000, span: int = 500_000) -> TierParams:
    return TierParams(rate, gain, quantum, span)


def test_monotonic_versions_with_content_hash(tmp_path: Path) -> None:
    store = ParameterStore(tmp_path / "p.json")
    v1 = store.submit("tier_a", _params(gain=256))
    v2 = store.submit("tier_a", _params(gain=300))
    assert v1.version == 1 and v2.version == 2
    assert v1.origin == "new"
    assert len(v1.content_hash) == 64
    assert v1.content_hash != v2.content_hash


def test_duplicate_submit_returns_existing_version(tmp_path: Path) -> None:
    store = ParameterStore(tmp_path / "p.json")
    v1 = store.submit("tier_a", _params())
    again = store.submit("tier_a", _params())
    assert again.version == v1.version
    assert len(store.versions("tier_a")) == 1


def test_rollback_creates_new_version_keeps_history(tmp_path: Path) -> None:
    store = ParameterStore(tmp_path / "p.json")
    v1 = store.submit("tier_a", _params(gain=256))
    store.submit("tier_a", _params(gain=300))
    rolled = store.rollback("tier_a", v1.version)
    assert rolled.version == 3
    assert rolled.origin == "rollback"
    assert rolled.rolled_back_from == 1
    assert rolled.content_hash == v1.content_hash
    assert [v.version for v in store.versions("tier_a")] == [1, 2, 3]


def test_snapshot_roundtrip(tmp_path: Path) -> None:
    path = tmp_path / "p.json"
    store = ParameterStore(path)
    store.submit("tier_a", _params(gain=256))
    store.submit("tier_a", _params(gain=300))
    reloaded = ParameterStore(path)
    assert reloaded.latest("tier_a").params.gain_q8 == 300
    assert reloaded.latest("tier_a").content_hash == store.latest("tier_a").content_hash


def test_invalid_span_not_multiple_of_quantum(tmp_path: Path) -> None:
    store = ParameterStore(tmp_path / "p.json")
    with pytest.raises(Exception) as exc:
        store.submit("tier_a", _params(quantum=1000, span=500_100))
    assert exc.value.code == ErrorCode.E_STATE_INVALID
