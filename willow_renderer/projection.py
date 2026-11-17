"""Perspective projection of weave holes onto outer layers and the inner wall.

For every (hole, lamp) pair the hole cell quad is centrally projected
through the lamp position onto each receiver plane (the next outer layer
and the lantern inner wall).  Projected quads are rasterised into a
per-receiver light field with a soft penumbra whose width grows with the
air-gap thickness and the lamp-to-hole distance.  Landings outside the
receiver extent are treated as unoccluded (no spot, no dark edge).
"""

import numpy as np


class ProjectionResult:
    def __init__(self, fields, field_extents, landings, penumbra_samples):
        self.fields = fields                # {(kind, layer_id): (res, res, 3) float64}
        self.field_extents = field_extents  # {(kind, layer_id): extent}
        self.landings = landings            # list of landing dicts (report)
        self.penumbra_samples = penumbra_samples  # list of measurement dicts


def _hole_corners(layer, hole):
    panel = layer["panel"]
    cu, cv = hole["cell"]
    w = panel["width"] / panel["cells_u"]
    h = panel["height"] / panel["cells_v"]
    x0 = (cu / panel["cells_u"] - 0.5) * panel["width"]
    y0 = (cv / panel["cells_v"] - 0.5) * panel["height"]
    z = layer["plane_z"]
    return np.array([
        [x0, y0, z],
        [x0 + w, y0, z],
        [x0 + w, y0 + h, z],
        [x0, y0 + h, z],
    ], dtype=np.float64)


def _project_quad(corners, lamp_pos, receiver_z):
    denom = corners[0, 2] - lamp_pos[2]
    if abs(denom) < 1e-9:
        return None
    t = (receiver_z - lamp_pos[2]) / denom
    return lamp_pos[None, :] + t * (corners - lamp_pos[None, :])


def _soft_mask(poly2d, xs, ys, penumbra):
    """Soft coverage of a convex quad at sample points (vectorised).

    Returns values in [0, 1]; smoothstep-like transition of width
    ``penumbra`` centred on the quad boundary.
    """
    px = xs.ravel()[None, :]
    py = ys.ravel()[None, :]
    n = poly2d.shape[0]
    min_dist = np.full(px.shape[1], np.inf)
    signs = []
    for i in range(n):
        ax, ay = poly2d[i]
        bx, by = poly2d[(i + 1) % n]
        ex = bx - ax
        ey = by - ay
        cross = ex * (py - ay) - ey * (px - ax)
        edge_len = np.hypot(ex, ey)
        dist = np.abs(cross) / max(edge_len, 1e-12)
        min_dist = np.minimum(min_dist, dist.ravel())
        signs.append(cross)
    signs = np.stack(signs, axis=0)
    inside = np.logical_or(np.all(signs >= 0.0, axis=0), np.all(signs <= 0.0, axis=0)).ravel()
    signed = np.where(inside, -min_dist, min_dist)
    cov = np.clip(0.5 - signed / penumbra, 0.0, 1.0)
    return cov.reshape(xs.shape)


def _rasterise_spot(field, extent, poly2d, lamp_pos, receiver_z, lamp_rgb,
                    penumbra, density, throughput):
    res = field.shape[0]
    texel = extent / res
    pad = penumbra + texel
    lo = np.floor((poly2d.min(axis=0) - pad + extent * 0.5) / texel).astype(np.int64)
    hi = np.ceil((poly2d.max(axis=0) + pad + extent * 0.5) / texel).astype(np.int64)
    lo = np.clip(lo, 0, res - 1)
    hi = np.clip(hi, 0, res)
    if hi[0] <= lo[0] or hi[1] <= lo[1]:
        return None
    sub = (np.arange(density, dtype=np.float64) + 0.5) / density - 0.5
    # Build supersampled coordinate grids for the texel block.
    base_x = (np.arange(lo[0], hi[0], dtype=np.float64) + 0.5) * texel - extent * 0.5
    base_y = (np.arange(lo[1], hi[1], dtype=np.float64) + 0.5) * texel - extent * 0.5
    acc = np.zeros((hi[1] - lo[1], hi[0] - lo[0]), dtype=np.float64)
    count = 0
    for sy in sub:
        for sx in sub:
            xs = base_x[None, :] + sx * texel
            ys = base_y[:, None] + sy * texel
            xs2 = np.broadcast_to(xs, (ys.shape[0], xs.shape[1]))
            ys2 = np.broadcast_to(ys, (ys.shape[0], xs.shape[1]))
            acc += _soft_mask(poly2d, xs2, ys2, penumbra)
            count += 1
    acc /= count
    cx = base_x[None, :]
    cy = base_y[:, None]
    dist2 = ((cx - lamp_pos[0]) ** 2 + (cy - lamp_pos[1]) ** 2
             + (receiver_z - lamp_pos[2]) ** 2)
    atten = 1.0 / (1.0 + dist2)
    block = acc * atten * throughput
    field[lo[1]:hi[1], lo[0]:hi[0], :] += block[:, :, None] * lamp_rgb[None, None, :]
    brightness = float((block.max()) * float(lamp_rgb.sum()))
    return {"coverage": acc, "lo": lo, "texel": texel, "extent": extent,
            "brightness": brightness}


def _measure_penumbra(spot, poly2d):
    """Measure 10%-90% edge transition width on the isolated spot coverage."""
    coverage = spot["coverage"]
    texel = spot["texel"]
    lo = spot["lo"]
    centroid = poly2d.mean(axis=0)
    # Centroid in block-local texel coordinates.
    res_x = (centroid[0] + spot["extent"] * 0.5) / texel - lo[0]
    res_y = (centroid[1] + spot["extent"] * 0.5) / texel - lo[1]
    row = int(np.clip(round(res_y), 0, coverage.shape[0] - 1))
    profile = coverage[row, :]
    peak = profile.max()
    if peak <= 1e-12:
        return None

    def crossing(level, start, step):
        """Sub-texel x position where the profile crosses ``level``."""
        prev = profile[start]
        idx = start
        while 0 <= idx < profile.shape[0]:
            cur = profile[idx]
            if (prev - level) * (cur - level) <= 0.0 and cur != prev:
                frac = (level - prev) / (cur - prev)
                return (idx - 1 + frac) * texel
            prev = cur
            idx += step
        return None

    peak_idx = int(np.argmax(profile))
    left90 = crossing(0.9 * peak, peak_idx, -1)
    left10 = crossing(0.1 * peak, peak_idx, -1)
    right90 = crossing(0.9 * peak, peak_idx, +1)
    right10 = crossing(0.1 * peak, peak_idx, +1)
    if None in (left90, left10, right90, right10):
        return None
    return {
        "centroid_brightness": spot["brightness"],
        "edge_width_left": float(left90 - left10),
        "edge_width_right": float(right10 - right90),
    }


def compute_light_fields(scene):
    layers = sorted(scene["layers"], key=lambda item: item["plane_z"])
    proj = scene["projection"]
    res = proj["field_resolution"]
    density = proj["edge_sample_density"]
    softness = proj["softness"]
    lamps = scene["lamps"]
    wall = scene["wall"]

    receivers = []
    for layer in layers[1:]:
        extent = max(layer["panel"]["width"], layer["panel"]["height"])
        receivers.append((("layer", layer["layer_id"]), layer["plane_z"], extent))
    receivers.append((("wall", -1), wall["z"], wall["extent"]))

    fields = {key: np.zeros((res, res, 3), dtype=np.float64) for key, _, _ in receivers}
    extents = {key: extent for key, _, extent in receivers}
    landings = []
    measurements = []

    trans_by_layer = {layer["layer_id"]: layer["transmittance"] for layer in layers}

    for key, receiver_z, extent in receivers:
        receiver_kind, receiver_layer = key
        for li, layer in enumerate(layers):
            if receiver_kind == "layer" and layer["plane_z"] >= receiver_z:
                continue
            for hole in layer.get("holes", []):
                corners = _hole_corners(layer, hole)
                hole_centroid = corners.mean(axis=0)
                for lamp in lamps:
                    lamp_pos = np.asarray(lamp["position"], dtype=np.float64)
                    lamp_rgb = np.asarray(lamp["intensity"], dtype=np.float64)
                    poly = _project_quad(corners, lamp_pos, receiver_z)
                    if poly is None:
                        continue
                    centroid = poly[:, :2].mean(axis=0)
                    in_range = bool(np.all(np.abs(centroid) <= extent * 0.5))
                    lamp_dist = float(np.linalg.norm(hole_centroid - lamp_pos))
                    gap = abs(receiver_z - layer["plane_z"])
                    landing = {
                        "lamp_id": lamp["lamp_id"],
                        "hole_id": hole["hole_id"],
                        "from_layer": layer["layer_id"],
                        "receiver": ("wall" if receiver_kind == "wall"
                                     else f"layer_{receiver_layer}"),
                        "x": float(centroid[0]),
                        "y": float(centroid[1]),
                        "in_range": in_range,
                        "air_gap": float(gap),
                        "lamp_distance": lamp_dist,
                    }
                    landings.append(landing)
                    if not in_range:
                        # Outside the receiver: unoccluded, no spot, no dark edge.
                        continue
                    throughput = 1.0
                    for mid_layer in layers[li + 1:]:
                        if receiver_kind == "layer" and mid_layer["plane_z"] >= receiver_z:
                            break
                        throughput *= trans_by_layer[mid_layer["layer_id"]]
                    penumbra = softness * (gap + lamp_dist)
                    spot = _rasterise_spot(fields[key], extent, poly[:, :2], lamp_pos,
                                           receiver_z, lamp_rgb, penumbra, density,
                                           throughput)
                    if spot is not None:
                        measurement = _measure_penumbra(spot, poly[:, :2])
                        if measurement is not None:
                            measurement.update({
                                "lamp_id": lamp["lamp_id"],
                                "hole_id": hole["hole_id"],
                                "from_layer": layer["layer_id"],
                                "receiver": landing["receiver"],
                                "air_gap": float(gap),
                                "lamp_distance": lamp_dist,
                                "nominal_penumbra": float(penumbra),
                            })
                            measurements.append(measurement)

    return ProjectionResult(fields, extents, landings, measurements)


def sample_field(field, extent, xs, ys):
    """Bilinear sample of a receiver light field at world coordinates."""
    res = field.shape[0]
    texel = extent / res
    fx = (xs + extent * 0.5) / texel - 0.5
    fy = (ys + extent * 0.5) / texel - 0.5
    x0 = np.clip(np.floor(fx).astype(np.int64), 0, res - 2)
    y0 = np.clip(np.floor(fy).astype(np.int64), 0, res - 2)
    tx = np.clip(fx - x0, 0.0, 1.0)
    ty = np.clip(fy - y0, 0.0, 1.0)
    c00 = field[y0, x0]
    c10 = field[y0, x0 + 1]
    c01 = field[y0 + 1, x0]
    c11 = field[y0 + 1, x0 + 1]
    top = c00 * (1.0 - tx)[:, None] + c10 * tx[:, None]
    bot = c01 * (1.0 - tx)[:, None] + c11 * tx[:, None]
    return top * (1.0 - ty)[:, None] + bot * ty[:, None]
