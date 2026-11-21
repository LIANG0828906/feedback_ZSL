from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Callable

import pytest

from volrender.binary import encode_scene
from volrender.injection import LoadedInputs, _read_scene_container, load_inputs
from volrender.samplegen import write_sample
from volrender.scene import Scene, build_scene


@pytest.fixture(scope="session")
def sample_bundle(tmp_path_factory: pytest.TempPathFactory):
    root = tmp_path_factory.mktemp("volrender-sample")
    scene_path, config_path = write_sample(root, sample_budget=12000)
    loaded = load_inputs(scene_path, config_path)
    return {
        "root": root,
        "scene_path": scene_path,
        "config_path": config_path,
        "loaded": loaded,
        "config_data": json.loads(config_path.read_text(encoding="utf-8")),
    }


def rewrite_header(
    source: Path,
    destination: Path,
    mutate: Callable[[dict[str, Any]], None],
) -> Path:
    header, payload = _read_scene_container(source.read_bytes())
    mutate(header)
    destination.write_bytes(encode_scene(header, payload))
    return destination


def rewrite_payload(
    source: Path,
    destination: Path,
    mutate: Callable[[bytearray], None],
) -> Path:
    header, payload = _read_scene_container(source.read_bytes())
    mutable = bytearray(payload)
    mutate(mutable)
    destination.write_bytes(encode_scene(header, bytes(mutable)))
    return destination


def scene_from_header_payload(
    source: Path,
    mutate_header: Callable[[dict[str, Any]], None] | None = None,
    mutate_payload: Callable[[bytearray], None] | None = None,
) -> Scene:
    header, payload = _read_scene_container(source.read_bytes())
    if mutate_header is not None:
        mutate_header(header)
    mutable = bytearray(payload)
    if mutate_payload is not None:
        mutate_payload(mutable)
    return build_scene(header, bytes(mutable))
