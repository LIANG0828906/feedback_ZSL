"""Deterministic sample scene generator.

Builds a five-layer woven willow lantern scene: each layer is a pair of
interleaved ribbon families (horizontal / vertical strands) with
alternating over/under offsets, deterministic hole cells, per-layer
dye/transmittance, three oil lamps, an inner wall plane and an orbiting
camera.  Same seed -> byte-identical JSON.
"""

import json

import numpy as np


def _build_weave_mesh(plane_z, panel, holes):
    cells_u = panel["cells_u"]
    cells_v = panel["cells_v"]
    width = panel["width"]
    height = panel["height"]
    strand = panel["strand_width"]
    offset = panel["weave_offset"]
    cw = width / cells_u
    ch = height / cells_v
    sw = strand * cw
    sh = strand * ch

    hole_cells = {tuple(h["cell"]): h["hole_id"] for h in holes}
    vertices = []
    triangles = []
    tri_hole_ids = []

    def add_quad(x0, y0, x1, y1, z, hole_id):
        base = len(vertices)
        vertices.extend([
            [x0, y0, z],
            [x1, y0, z],
            [x1, y1, z],
            [x0, y1, z],
        ])
        triangles.append([base, base + 1, base + 2])
        triangles.append([base, base + 2, base + 3])
        tri_hole_ids.extend([hole_id, hole_id])

    def border_hole_id(cu, cv):
        hid = -1
        for nb in ((cu - 1, cv), (cu + 1, cv), (cu, cv - 1), (cu, cv + 1)):
            if nb in hole_cells:
                hid = hole_cells[nb] if hid == -1 else min(hid, hole_cells[nb])
        return hid

    # Horizontal ribbons (one per row), segmented per cell.
    for cv in range(cells_v):
        yc = (cv + 0.5) * ch - height * 0.5
        y0, y1 = yc - sh * 0.5, yc + sh * 0.5
        for cu in range(cells_u):
            if (cu, cv) in hole_cells:
                continue
            x0 = cu * cw - width * 0.5
            z = plane_z + (offset if (cu + cv) % 2 == 0 else -offset)
            add_quad(x0, y0, x0 + cw, y1, z, border_hole_id(cu, cv))

    # Vertical ribbons (one per column), opposite over/under parity.
    for cu in range(cells_u):
        xc = (cu + 0.5) * cw - width * 0.5
        x0, x1 = xc - sw * 0.5, xc + sw * 0.5
        for cv in range(cells_v):
            if (cu, cv) in hole_cells:
                continue
            y0 = cv * ch - height * 0.5
            z = plane_z + (-offset if (cu + cv) % 2 == 0 else offset)
            add_quad(x0, y0, x1, y0 + ch, z, border_hole_id(cu, cv))

    return {"vertices": vertices, "triangles": triangles, "tri_hole_ids": tri_hole_ids}


def generate_sample_scene(seed=20261118, image_size=256, tile_size=64):
    n_layers = 5
    air_gaps = [0.45, 0.55, 0.65, 0.75]
    dyes = [
        [0.86, 0.55, 0.24],
        [0.80, 0.48, 0.22],
        [0.74, 0.44, 0.24],
        [0.68, 0.40, 0.26],
        [0.62, 0.38, 0.30],
    ]
    layers = []
    plane_z = 0.0
    for lid in range(n_layers):
        panel = {
            "width": 4.0 + 0.35 * lid,
            "height": 4.0 + 0.35 * lid,
            "cells_u": 10,
            "cells_v": 10,
            "strand_width": 0.62,
            "weave_offset": 0.05,
        }
        layer_seed = int(seed + 101 * lid)
        layer_rng = np.random.Generator(np.random.PCG64(layer_seed))
        interior = [c for c in range(100) if 1 <= c % 10 <= 8 and 1 <= c // 10 <= 8]
        n_holes = 4 + (lid % 2)
        picked = layer_rng.choice(interior, size=n_holes, replace=False)
        holes = [
            {"hole_id": hid, "cell": [int(cell % 10), int(cell // 10)]}
            for hid, cell in enumerate(sorted(int(c) for c in picked))
        ]
        mesh = _build_weave_mesh(plane_z, panel, holes)
        layers.append({
            "layer_id": lid,
            "seed": layer_seed,
            "plane_z": round(plane_z, 6),
            "thickness": 0.12,
            "panel": panel,
            "transmittance": round(0.40 + 0.04 * lid, 4),
            "dye_color": dyes[lid],
            "holes": holes,
            "mesh": mesh,
        })
        if lid < n_layers - 1:
            plane_z += air_gaps[lid]

    return {
        "scene_id": f"willow-lantern-{seed}",
        "seed": seed,
        "image": {"width": image_size, "height": image_size},
        "tile": {"size": tile_size},
        "camera": {
            "position": [2.4, 1.1, 8.6],
            "target": [0.0, 0.0, 1.1],
            "up": [0.0, 1.0, 0.0],
            "fov_deg": 40.0,
        },
        "wall": {"z": -3.0, "extent": 8.0, "albedo": [0.72, 0.68, 0.60]},
        "projection": {
            "edge_sample_density": 4,
            "softness": 0.05,
            "field_resolution": 96,
        },
        "shading": {
            "reflect_weight": 0.9,
            "transmit_weight": 0.8,
            "ambient": 0.05,
        },
        "air_gaps": air_gaps,
        "lamps": [
            {"lamp_id": 0, "position": [-1.3, -0.9, -1.6], "intensity": [6.0, 4.2, 2.2]},
            {"lamp_id": 1, "position": [1.5, -0.3, -1.1], "intensity": [5.0, 3.8, 2.0]},
            {"lamp_id": 2, "position": [0.2, 1.4, -2.1], "intensity": [5.5, 4.0, 2.4]},
        ],
        "layers": layers,
    }


def write_sample_scene(path, seed=20261118, image_size=256, tile_size=64):
    scene = generate_sample_scene(seed=seed, image_size=image_size, tile_size=tile_size)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(scene, fh, ensure_ascii=False, indent=1)
        fh.write("\n")
    return scene
