"""Deterministic per-layer weave texture tables and hole mask tables.

Each layer derives its tables solely from its integer ``seed`` via a
seeded PCG64 generator, so repeated generation is byte-identical and
independent of layer traversal order.
"""

import numpy as np


class LayerTables:
    __slots__ = ("layer_id", "color_mod", "trans_mod", "hole_mask", "over_under")

    def __init__(self, layer_id, color_mod, trans_mod, hole_mask, over_under):
        self.layer_id = layer_id
        self.color_mod = color_mod
        self.trans_mod = trans_mod
        self.hole_mask = hole_mask
        self.over_under = over_under


def build_layer_tables(layer):
    """Build the weave texture sampling table and hole mask for one layer."""
    panel = layer["panel"]
    cells_u = panel["cells_u"]
    cells_v = panel["cells_v"]
    rng = np.random.Generator(np.random.PCG64(layer["seed"]))

    color_mod = 1.0 + 0.12 * rng.standard_normal((cells_v, cells_u))
    color_mod = np.clip(color_mod, 0.78, 1.22)
    trans_mod = 1.0 + 0.18 * rng.standard_normal((cells_v, cells_u))
    trans_mod = np.clip(trans_mod, 0.7, 1.3)

    hole_mask = np.zeros((cells_v, cells_u), dtype=bool)
    for hole in layer.get("holes", []):
        cu, cv = hole["cell"]
        hole_mask[cv, cu] = True

    u_idx, v_idx = np.meshgrid(np.arange(cells_u), np.arange(cells_v))
    over_under = ((u_idx + v_idx) % 2).astype(np.int8)

    return LayerTables(layer["layer_id"], color_mod, trans_mod, hole_mask, over_under)


def build_all_tables(scene):
    """Tables keyed by layer_id; order-independent because each is self-seeded."""
    return {layer["layer_id"]: build_layer_tables(layer) for layer in scene["layers"]}


def sample_tables(tables, panel, xs, ys):
    """Nearest-cell sample of the weave tables at world panel coordinates.

    ``xs``/``ys`` are world x/y of fragments on the layer plane. Returns
    (color_mod, trans_mod, is_hole) arrays. Nearest lookup keeps hole mask
    borders crisp.
    """
    cells_u = panel["cells_u"]
    cells_v = panel["cells_v"]
    fu = (xs / panel["width"] + 0.5) * cells_u
    fv = (ys / panel["height"] + 0.5) * cells_v
    iu = np.clip(np.floor(fu).astype(np.int64), 0, cells_u - 1)
    iv = np.clip(np.floor(fv).astype(np.int64), 0, cells_v - 1)
    return (
        tables.color_mod[iv, iu],
        tables.trans_mod[iv, iu],
        tables.hole_mask[iv, iu],
    )
