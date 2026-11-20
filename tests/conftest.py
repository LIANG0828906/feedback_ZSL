from __future__ import annotations

import json
import struct
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "artifacts" / "acceptance" / "samples"


@pytest.fixture
def sample_dir() -> Path:
    return SAMPLES


@pytest.fixture
def smoke_path(sample_dir: Path) -> Path:
    return sample_dir / "smoke_small.dust"


@pytest.fixture
def nested_path(sample_dir: Path) -> Path:
    return sample_dir / "nested_medium.dust"


@pytest.fixture
def triple_path(sample_dir: Path) -> Path:
    return sample_dir / "triple_wide.dust"


def unpack_scene(path: Path) -> tuple[dict, bytes]:
    from dustrender.scene import HEADER_SIZE, MAGIC
    from dustrender.util import sha256_bytes

    data = path.read_bytes()
    assert data[:8] == MAGIC
    _, metadata_length, payload_length = struct.unpack_from("<III", data, 8)
    metadata = json.loads(data[HEADER_SIZE : HEADER_SIZE + metadata_length])
    payload = data[HEADER_SIZE + metadata_length :]
    assert len(payload) == payload_length
    del sha256_bytes
    return metadata, payload


def pack_scene(metadata: dict, payload: bytes, *, corrupt_hash: bool = False) -> bytes:
    from dustrender.scene import HEADER_SIZE, MAGIC
    from dustrender.util import canonical_json_bytes, sha256_bytes

    metadata_bytes = canonical_json_bytes(metadata)
    payload_hash = sha256_bytes(payload)
    if corrupt_hash:
        payload_hash = "00" * 32
    assert HEADER_SIZE == 52
    return (
        MAGIC
        + struct.pack("<III", 1, len(metadata_bytes), len(payload))
        + bytes.fromhex(payload_hash)
        + metadata_bytes
        + payload
    )
