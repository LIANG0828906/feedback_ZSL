from __future__ import annotations

import threading
import time
from pathlib import Path

import numpy as np

from mesh_repair.binary import load_mesh
from mesh_repair.render import RenderLoop, write_static_preview
from mesh_repair.snapshot import SnapshotStore


def test_snapshot_replacement_is_whole_and_immutable() -> None:
    store = SnapshotStore()
    vertices = np.asarray([[0, 0, 0], [1, 0, 0], [0, 1, 0]], dtype=np.float64)
    faces = np.asarray([[0, 1, 2]], dtype=np.int64)
    first = store.publish(vertices, faces)
    vertices[0, 0] = 99.0
    assert first.vertices[0, 0] == 0.0
    try:
        first.faces[0, 0] = 2
    except ValueError:
        pass
    else:
        raise AssertionError("snapshot arrays must be read-only")
    second = store.publish(vertices, faces)
    assert second.version == first.version + 1
    assert store.latest() is second


def test_renderer_uses_latest_snapshot_or_static_fallback(
    sample_bundle: dict, tmp_path: Path
) -> None:
    mesh, _ = load_mesh(sample_bundle["path"])
    store = SnapshotStore()
    block_ids = np.zeros(mesh.face_count, dtype=np.int64)
    store.publish(mesh.vertices, mesh.faces, face_block_ids=block_ids)
    stop = threading.Event()
    loop = RenderLoop(
        store,
        stop_event=stop,
        preview_path=tmp_path / "preview.png",
        width=320,
        height=240,
    )
    thread = threading.Thread(target=loop.run)
    thread.start()
    time.sleep(0.25)
    stop.set()
    thread.join(timeout=10)
    assert loop.status is not None
    assert loop.status.mode in {"opengl-offscreen", "static-fallback"}
    assert loop.status.preview_path is not None
    assert Path(loop.status.preview_path).exists()


def test_static_preview_fallback(sample_bundle: dict, tmp_path: Path) -> None:
    mesh, _ = load_mesh(sample_bundle["path"])
    store = SnapshotStore()
    snapshot = store.publish(mesh.vertices, mesh.faces)
    output = write_static_preview(snapshot, tmp_path / "fallback.svg")
    assert output.endswith(".svg")
    assert "<svg" in Path(output).read_text()
