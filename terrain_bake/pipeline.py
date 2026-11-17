"""End-to-end chunked terrain bake with crash-safe resume."""

import hashlib
import json
import os
import time

import numpy as np

from . import checkpoint as ck
from .chunks import (build_grid, chunk_bbox, chunk_origin, iter_chunks,
                     neighbors)
from .heightfield import generate_heights, heights_digest
from .normals import (chunk_normals_local, chunk_normals_with_halo,
                      repair_seams)
from .texture import bake_texture, encode_png

CHUNK_FIELDS = ("positions", "normals", "uv", "texture")


def default_params(seed=1337, chunk_verts=17, chunks_x=4, chunks_y=4,
                   world_size_x=256.0, world_size_z=256.0,
                   height_scale=24.0, feature=8, octaves=3):
    return {
        "seed": int(seed),
        "chunk_verts": int(chunk_verts),
        "chunks_x": int(chunks_x),
        "chunks_y": int(chunks_y),
        "world_size_x": float(world_size_x),
        "world_size_z": float(world_size_z),
        "height_scale": float(height_scale),
        "feature": int(feature),
        "octaves": int(octaves),
    }


def _chunk_dir(out_dir, cx, cy):
    return os.path.join(out_dir, "chunks", f"chunk_{cx}_{cy}")


def _file_digest(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def _verify_committed_outputs(out_dir):
    def verify(coord, digests):
        cdir = _chunk_dir(out_dir, coord[0], coord[1])
        paths = {
            "positions": os.path.join(cdir, "positions.npy"),
            "normals": os.path.join(cdir, "normals.npy"),
            "uv": os.path.join(cdir, "uv.npy"),
            "texture": os.path.join(cdir, "texture.png"),
        }
        for field, path in paths.items():
            if not os.path.exists(path):
                return False
            if _file_digest(path) != digests.get(field):
                return False
        return True
    return verify


def _write_chunk_outputs(cdir, positions, normals, uv, png_bytes):
    os.makedirs(cdir, exist_ok=True)
    with open(os.path.join(cdir, "positions.npy"), "wb") as fh:
        np.save(fh, positions)
    with open(os.path.join(cdir, "normals.npy"), "wb") as fh:
        np.save(fh, normals)
    with open(os.path.join(cdir, "uv.npy"), "wb") as fh:
        np.save(fh, uv)
    with open(os.path.join(cdir, "texture.png"), "wb") as fh:
        fh.write(png_bytes)
    return {
        "positions": _file_digest(os.path.join(cdir, "positions.npy")),
        "normals": _file_digest(os.path.join(cdir, "normals.npy")),
        "uv": _file_digest(os.path.join(cdir, "uv.npy")),
        "texture": _file_digest(os.path.join(cdir, "texture.png")),
    }


def run_bake(params, out_dir, crash_after=None):
    """Bake all chunks into `out_dir`, resuming from a valid snapshot.

    `crash_after` (test hook) hard-kills the process with os._exit after the
    given number of chunks has been committed, simulating SIGKILL.
    Returns the bake report dict.
    """
    started = time.monotonic()
    os.makedirs(out_dir, exist_ok=True)
    grid = build_grid(params["chunk_verts"], params["chunks_x"],
                      params["chunks_y"])
    digest = ck.input_digest(params)

    heights = generate_heights(params["seed"], grid.verts_x, grid.verts_y,
                               params["feature"], params["octaves"])
    h_digest = heights_digest(heights)

    checkpoint = ck.load_checkpoint(
        out_dir, digest, verify_outputs=_verify_committed_outputs(out_dir))
    if checkpoint is None:
        checkpoint = ck.Checkpoint(out_dir, digest, h_digest)
        checkpoint.commit()

    step_x = params["world_size_x"] / (grid.verts_x - 1)
    step_z = params["world_size_z"] / (grid.verts_y - 1)
    n = grid.chunk_verts

    manifest_chunks = []
    total_repaired = 0
    committed_this_run = 0

    for cx, cy in iter_chunks(grid):
        ox, oy = chunk_origin(grid, cx, cy)
        cdir = _chunk_dir(out_dir, cx, cy)
        if (cx, cy) in checkpoint.completed:
            digests = next(c["digests"] for c in
                           checkpoint.payload["completed_chunks"]
                           if c["coord"] == [cx, cy])
        else:
            chunk_h = heights[oy:oy + n, ox:ox + n]
            gx = ox + np.arange(n, dtype=np.float64)
            gy = oy + np.arange(n, dtype=np.float64)
            xs = gx * step_x
            zs = gy * step_z
            ys = chunk_h * params["height_scale"]
            positions = np.stack([
                np.broadcast_to(xs[None, :], (n, n)),
                ys,
                np.broadcast_to(zs[:, None], (n, n)),
            ], axis=-1).astype(np.float64)
            uv = np.stack([
                np.broadcast_to((gx / (grid.verts_x - 1))[None, :], (n, n)),
                np.broadcast_to((gy / (grid.verts_y - 1))[:, None], (n, n)),
            ], axis=-1).astype(np.float64)

            local = chunk_normals_local(chunk_h, step_x, step_z)
            halo = chunk_normals_with_halo(heights, ox, oy, n, step_x, step_z)
            repaired, repaired_count = repair_seams(local, halo)
            total_repaired += repaired_count

            png_bytes = encode_png(bake_texture(repaired))
            digests = _write_chunk_outputs(cdir, positions, repaired, uv,
                                           png_bytes)
            checkpoint.record_chunk((cx, cy), digests)
            checkpoint.set_seam_repair(
                checkpoint.payload["seam_repair"]["repaired_boundary_vertices"]
                + repaired_count)
            checkpoint.commit()
            committed_this_run += 1
            if crash_after is not None and committed_this_run >= crash_after:
                os._exit(1)  # simulate SIGKILL: no cleanup, no finally

        positions = np.load(os.path.join(cdir, "positions.npy"))
        manifest_chunks.append({
            "coord": [cx, cy],
            "vertices": [n, n],
            "origin_vertex": [int(ox), int(oy)],
            "bbox": chunk_bbox(positions),
            "neighbors": neighbors(grid, cx, cy),
            "outputs": digests,
        })

    if not checkpoint.payload["seam_repair"]["done"]:
        checkpoint.set_seam_repair(total_repaired)
    checkpoint.mark_complete()
    checkpoint.commit()

    manifest = {
        "grid": {
            "chunk_verts": grid.chunk_verts,
            "chunks_x": grid.chunks_x,
            "chunks_y": grid.chunks_y,
            "verts_x": grid.verts_x,
            "verts_y": grid.verts_y,
            "chunk_count": grid.chunk_count,
        },
        "chunks": manifest_chunks,
    }
    with open(os.path.join(out_dir, "chunks.json"), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2)
        fh.write("\n")

    report = {
        "seed": params["seed"],
        "input_digest": digest,
        "heights_digest": h_digest,
        "grid": manifest["grid"],
        "total_chunks": grid.chunk_count,
        "total_vertices": grid.total_vertices,
        "repaired_boundary_vertices":
            checkpoint.payload["seam_repair"]["repaired_boundary_vertices"],
        "resume_count": checkpoint.payload["resume_count"],
        "texture_hashes": [
            {"coord": c["coord"], "sha256": c["outputs"]["texture"]}
            for c in manifest_chunks
        ],
        "elapsed_ms": int((time.monotonic() - started) * 1000),
    }
    with open(os.path.join(out_dir, "report.json"), "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2)
        fh.write("\n")
    return report
