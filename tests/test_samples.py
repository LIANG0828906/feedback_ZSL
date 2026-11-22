from __future__ import annotations

import hashlib
from pathlib import Path

import pytest

from edgepipe.errors import EdgePipeError, ErrorCode
from edgepipe.samples import (
    decode_image,
    generate_samples,
    load_manifest,
    sample_image_path,
)


def _file_hashes(directory: Path):
    return {
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(directory.iterdir())
        if path.is_file()
    }


def test_sample_manifest_has_required_coverage(manifest):
    samples = manifest["samples"]
    assert len(samples) >= 4
    assert {item["channels"] for item in samples} == {1, 3}
    assert len({(item["width"], item["height"]) for item in samples}) >= 4
    assert any(item["scene"] == "noisy" for item in samples)
    for item in samples:
        assert item["segments"]
        assert item["circles"]
        for segment in item["segments"]:
            assert {"x1", "y1", "x2", "y2"} <= set(segment)
        for circle in item["circles"]:
            assert {"cx", "cy", "diameter"} <= set(circle)


def test_sample_generation_is_byte_deterministic(tmp_path: Path):
    first = tmp_path / "first"
    second = tmp_path / "second"
    generate_samples(first, clean=True)
    generate_samples(second, clean=True)
    assert _file_hashes(first) == _file_hashes(second)


def test_generated_samples_decode(samples_dir: Path, manifest):
    for item in manifest["samples"]:
        image = decode_image(sample_image_path(item, samples_dir))
        assert image.shape[:2] == (item["height"], item["width"])
        assert (1 if image.ndim == 2 else image.shape[2]) == item["channels"]


def test_corrupt_image_returns_decode_error(tmp_path: Path):
    path = tmp_path / "corrupt.png"
    path.write_bytes(b"broken image payload")
    with pytest.raises(EdgePipeError) as caught:
        decode_image(path)
    assert caught.value.code == ErrorCode.DECODE

