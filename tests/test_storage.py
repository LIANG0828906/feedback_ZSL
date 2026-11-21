"""要求 7：切片原子提交与强制终止恢复。"""

from __future__ import annotations
import json
from pathlib import Path

import pytest

from mediaaudit.errors import ErrorCode
from mediaaudit.parameters import TierParams
from mediaaudit.slicing import generate_slice
from mediaaudit.storage import GENESIS_HASH, SliceStore
from mediaaudit.util import content_hash
from tests.test_slicing import _source


def _artifact(version: int = 1, index: int = 0):
    source = _source()
    params = TierParams(16000, 256, 1000, 250_000)
    return generate_slice(source, "tier", version, params, index, 0, 250_000)


def test_atomic_write_leaves_no_temp_files(tmp_path: Path) -> None:
    store = SliceStore(tmp_path)
    artifact = _artifact()
    store.write_slice_atomic(artifact)
    assert not list(store.slices_dir.glob("*.tmp"))
    assert store.slice_path("tier", 1, 0).exists()


def test_manifest_hash_chain_and_recovery(tmp_path: Path) -> None:
    store = SliceStore(tmp_path)
    prev = GENESIS_HASH
    for index in range(3):
        artifact = _artifact(1, index)
        store.write_slice_atomic(artifact)
        row = artifact.manifest_row(graph_version=1)
        row["file_name"] = store.slice_path("tier", 1, index).name
        record = store.append_manifest_row(row, prev)
        prev = record["row_hash"]
    rows, removed = store.recover()
    assert len(rows) == 3
    assert removed == []


def test_broken_chain_aborts_with_recovery_invalid(tmp_path: Path) -> None:
    store = SliceStore(tmp_path)
    artifact = _artifact()
    store.write_slice_atomic(artifact)
    row = artifact.manifest_row(graph_version=1)
    row["file_name"] = "x"
    record = store.append_manifest_row(row, GENESIS_HASH)
    # 篡改第二行的前驱哈希。
    record["prev_hash"] = "f" * 64
    with store.manifest_path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(record) + "\n")
    with pytest.raises(Exception) as exc:
        store.load_manifest()
    assert exc.value.code == ErrorCode.E_RECOVERY_INVALID


def test_missing_field_aborts(tmp_path: Path) -> None:
    store = SliceStore(tmp_path)
    with store.manifest_path.open("w", encoding="utf-8") as handle:
        handle.write(json.dumps({"tier": "tier"}) + "\n")
    with pytest.raises(Exception) as exc:
        store.load_manifest()
    assert exc.value.code == ErrorCode.E_RECOVERY_INVALID
    assert "content_hash" in exc.value.details["missing"]


def test_corrupted_slice_content_detected(tmp_path: Path) -> None:
    store = SliceStore(tmp_path)
    artifact = _artifact()
    store.write_slice_atomic(artifact)
    row = artifact.manifest_row(graph_version=1)
    row["file_name"] = store.slice_path("tier", 1, 0).name
    record = store.append_manifest_row(row, GENESIS_HASH)
    # 破坏切片文件内容但保持文件名。
    path = store.slice_path("tier", 1, 0)
    payload = json.loads(path.read_text())
    payload["audio_hex"] = "00"
    path.write_text(json.dumps(payload))
    with pytest.raises(Exception) as exc:
        store.recover()
    assert exc.value.code == ErrorCode.E_RECOVERY_INVALID


def test_temp_files_cleaned_on_recovery(tmp_path: Path) -> None:
    store = SliceStore(tmp_path)
    (store.slices_dir / "leftover.tmp").write_bytes(b"x")
    _, removed = store.recover()
    assert len(removed) == 1
