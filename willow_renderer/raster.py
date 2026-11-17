"""Deterministic tiled software rasteriser, shading and compositing.

All arithmetic is float64 in fixed evaluation order; triangle traversal
order is fixed by the scene file, so two runs over the same input are
bit-identical regardless of tiling or interruption/resume.
"""

import numpy as np

from . import projection, texture


OWNER_WALL = 254
OWNER_BACKGROUND = 255
OWNER_ALPHA_THRESHOLD = 0.05
HOLE_ALPHA = 0.03


class Camera:
    def __init__(self, cam, width, height):
        self.pos = np.asarray(cam["position"], dtype=np.float64)
        target = np.asarray(cam["target"], dtype=np.float64)
        up = np.asarray(cam["up"], dtype=np.float64)
        self.fwd = target - self.pos
        self.fwd /= np.linalg.norm(self.fwd)
        right = np.cross(self.fwd, up)
        self.right = right / np.linalg.norm(right)
        self.up = np.cross(self.right, self.fwd)
        self.tan_half = np.tan(np.deg2rad(cam["fov_deg"]) * 0.5)
        self.width = width
        self.height = height
        self.aspect = width / height

    def project(self, points):
        rel = points - self.pos[None, :]
        zc = rel @ self.fwd
        sx = (rel @ self.right) / (zc * self.tan_half * self.aspect)
        sy = (rel @ self.up) / (zc * self.tan_half)
        px = (sx * 0.5 + 0.5) * self.width
        py = (0.5 - sy * 0.5) * self.height
        return px, py, zc

    def ray_directions(self, xs, ys):
        nx = (2.0 * (xs + 0.5) / self.width - 1.0) * self.tan_half * self.aspect
        ny = (1.0 - 2.0 * (ys + 0.5) / self.height) * self.tan_half
        dirs = (self.fwd[None, :]
                + nx[:, None] * self.right[None, :]
                + ny[:, None] * self.up[None, :])
        return dirs / np.linalg.norm(dirs, axis=1, keepdims=True)


def _tone(light):
    """Soft tone map: keeps lamp-facing inner layers from blowing out to white."""
    return light / (1.0 + light)


class ShadingContext:
    def __init__(self, scene, tables, proj_result):
        self.scene = scene
        self.tables = tables
        self.proj = proj_result
        self.lamps = [
            (np.asarray(l["position"], dtype=np.float64),
             np.asarray(l["intensity"], dtype=np.float64))
            for l in scene["lamps"]
        ]
        layers = sorted(scene["layers"], key=lambda item: item["plane_z"])
        self.inner_atten = {}
        acc = 1.0
        for layer in layers:
            self.inner_atten[layer["layer_id"]] = acc
            acc *= layer["transmittance"]
        self.wall_atten = acc
        shading = scene.get("shading", {})
        self.reflect_weight = float(shading.get("reflect_weight", 0.9))
        self.transmit_weight = float(shading.get("transmit_weight", 0.8))
        self.ambient = float(shading.get("ambient", 0.04))
        self.layer_stats = {
            layer["layer_id"]: {"transmission": 0.0, "reflection": 0.0}
            for layer in scene["layers"]
        }


def _shade_fragments(ctx, layer, positions, normals):
    """Shade covered fragments of one layer; returns premultiplied RGBA."""
    lid = layer["layer_id"]
    tables = ctx.tables[lid]
    dye = np.asarray(layer["dye_color"], dtype=np.float64)
    color_mod, trans_mod, is_hole = texture.sample_tables(
        tables, layer["panel"], positions[:, 0], positions[:, 1])

    refl = np.zeros((positions.shape[0], 3), dtype=np.float64)
    tran = np.zeros((positions.shape[0], 3), dtype=np.float64)
    for lamp_pos, lamp_rgb in ctx.lamps:
        to_lamp = lamp_pos[None, :] - positions
        dist2 = np.einsum("ij,ij->i", to_lamp, to_lamp)
        inv_dist = 1.0 / np.sqrt(dist2)
        ldir = to_lamp * inv_dist[:, None]
        atten = 1.0 / (1.0 + dist2)
        cos_f = np.einsum("ij,ij->i", normals, ldir)
        refl += lamp_rgb[None, :] * np.maximum(cos_f, 0.0)[:, None] * atten[:, None]
        tran += lamp_rgb[None, :] * np.maximum(-cos_f, 0.0)[:, None] * atten[:, None]
    tran *= ctx.inner_atten[lid]

    key = ("layer", lid)
    if key in ctx.proj.fields:
        extent = ctx.proj.field_extents[key]
        spot = projection.sample_field(ctx.proj.fields[key], extent,
                                       positions[:, 0], positions[:, 1])
        tran += spot

    refl_t = _tone(refl)
    tran_t = _tone(tran)

    dye_mod = dye[None, :] * color_mod[:, None]
    alpha = np.clip(1.0 - layer["transmittance"] * trans_mod, 0.0, 1.0)
    alpha = np.where(is_hole, HOLE_ALPHA, alpha)

    diffuse_rgb = dye_mod * (refl_t * ctx.reflect_weight + ctx.ambient)
    tint = 0.4 + 0.6 * dye_mod
    transmit_rgb = tran_t * ctx.transmit_weight * tint

    ctx.layer_stats[lid]["reflection"] += float(
        np.sum(diffuse_rgb * alpha[:, None]))
    ctx.layer_stats[lid]["transmission"] += float(
        np.sum(transmit_rgb * alpha[:, None]))

    premult = (diffuse_rgb + transmit_rgb) * alpha[:, None]
    return premult, alpha


def _shade_wall(ctx, scene, positions):
    wall = scene["wall"]
    albedo = np.asarray(wall["albedo"], dtype=np.float64)
    normal = np.array([0.0, 0.0, 1.0])
    light = np.zeros((positions.shape[0], 3), dtype=np.float64)
    for lamp_pos, lamp_rgb in ctx.lamps:
        to_lamp = lamp_pos[None, :] - positions
        dist2 = np.einsum("ij,ij->i", to_lamp, to_lamp)
        ldir = to_lamp * (1.0 / np.sqrt(dist2))[:, None]
        atten = 1.0 / (1.0 + dist2)
        cos_f = np.maximum(ldir @ normal, 0.0)
        light += lamp_rgb[None, :] * cos_f[:, None] * atten[:, None]
    light *= ctx.wall_atten
    key = ("wall", -1)
    spot = projection.sample_field(ctx.proj.fields[key], ctx.proj.field_extents[key],
                                   positions[:, 0], positions[:, 1])
    light += spot
    rgb = albedo[None, :] * (_tone(light) + ctx.ambient)
    return np.clip(rgb, 0.0, 1.0)


def _rasterise_layer_tile(ctx, layer, camera, tile):
    """Rasterise one layer into a tile; returns (premult_rgb, alpha, covered)."""
    tx0, ty0, tw, th = tile
    mesh = layer["mesh"]
    verts = np.asarray(mesh["vertices"], dtype=np.float64)
    tris = np.asarray(mesh["triangles"], dtype=np.int64)
    px, py, zc = camera.project(verts)

    out_rgb = np.zeros((th, tw, 3), dtype=np.float64)
    out_a = np.zeros((th, tw), dtype=np.float64)
    zbuf = np.full((th, tw), np.inf, dtype=np.float64)

    for ti in range(tris.shape[0]):
        i0, i1, i2 = tris[ti]
        if zc[i0] <= 1e-9 or zc[i1] <= 1e-9 or zc[i2] <= 1e-9:
            continue
        xs = (px[i0], px[i1], px[i2])
        ys = (py[i0], py[i1], py[i2])
        minx = max(int(np.floor(min(xs))), tx0)
        maxx = min(int(np.ceil(max(xs))), tx0 + tw)
        miny = max(int(np.floor(min(ys))), ty0)
        maxy = min(int(np.ceil(max(ys))), ty0 + th)
        if minx >= maxx or miny >= maxy:
            continue
        gx = np.arange(minx, maxx, dtype=np.float64) + 0.5
        gy = np.arange(miny, maxy, dtype=np.float64) + 0.5
        gxs, gys = np.meshgrid(gx, gy)
        x0, x1, x2 = xs
        y0, y1, y2 = ys
        area = (x1 - x0) * (y2 - y0) - (x2 - x0) * (y1 - y0)
        if abs(area) < 1e-12:
            continue
        l0 = ((x1 - gxs) * (y2 - gys) - (x2 - gxs) * (y1 - gys)) / area
        l1 = ((x2 - gxs) * (y0 - gys) - (x0 - gxs) * (y2 - gys)) / area
        l2 = ((x0 - gxs) * (y1 - gys) - (x1 - gxs) * (y0 - gys)) / area
        mask = (l0 >= -1e-12) & (l1 >= -1e-12) & (l2 >= -1e-12)
        if not mask.any():
            continue
        w0, w1, w2 = 1.0 / zc[i0], 1.0 / zc[i1], 1.0 / zc[i2]
        l0m, l1m, l2m = l0[mask], l1[mask], l2[mask]
        zinv = l0m * w0 + l1m * w1 + l2m * w2
        z = 1.0 / zinv

        local_y, local_x = np.nonzero(mask)
        global_flat = (miny - ty0 + local_y) * tw + (minx - tx0 + local_x)
        zb_flat = zbuf.ravel()
        nearer = z < zb_flat[global_flat]
        if not nearer.any():
            continue
        sel = np.nonzero(nearer)[0]
        global_flat = global_flat[sel]
        l0m, l1m, l2m, z = l0m[sel], l1m[sel], l2m[sel], z[sel]

        p0, p1, p2 = verts[i0], verts[i1], verts[i2]
        pos = ((l0m[:, None] * w0 * p0[None, :]
                + l1m[:, None] * w1 * p1[None, :]
                + l2m[:, None] * w2 * p2[None, :]) * z[:, None])
        normal = np.cross(p1 - p0, p2 - p0)
        normal = normal / np.linalg.norm(normal)
        to_cam = camera.pos - pos
        if float(np.mean(np.einsum("ij,j->i", to_cam, normal))) < 0.0:
            normal = -normal
        normals = np.broadcast_to(normal[None, :], pos.shape)

        premult, alpha = _shade_fragments(ctx, layer, pos, normals)
        zb_flat[global_flat] = z
        out_rgb.reshape(-1, 3)[global_flat] = premult
        out_a.ravel()[global_flat] = alpha

    covered = out_a > 0.0
    return out_rgb, out_a, covered


def _render_wall_tile(ctx, scene, camera, tile):
    tx0, ty0, tw, th = tile
    gx = np.arange(tx0, tx0 + tw, dtype=np.float64)
    gy = np.arange(ty0, ty0 + th, dtype=np.float64)
    gxs, gys = np.meshgrid(gx, gy)
    dirs = camera.ray_directions(gxs.ravel(), gys.ravel())
    wall_z = scene["wall"]["z"]
    dz = dirs[:, 2]
    t = np.full(dirs.shape[0], -1.0, dtype=np.float64)
    valid = np.abs(dz) > 1e-12
    t[valid] = (wall_z - camera.pos[2]) / dz[valid]
    hit = valid & (t > 0.0)
    rgb = np.zeros((dirs.shape[0], 3), dtype=np.float64)
    alpha = np.zeros(dirs.shape[0], dtype=np.float64)
    if hit.any():
        pos = camera.pos[None, :] + t[hit, None] * dirs[hit]
        half = scene["wall"]["extent"] * 0.5
        inside = (np.abs(pos[:, 0]) <= half) & (np.abs(pos[:, 1]) <= half)
        idx = np.nonzero(hit)[0][inside]
        if idx.size:
            rgb[idx] = _shade_wall(ctx, scene, pos[inside])
            alpha[idx] = 1.0
    return rgb.reshape(th, tw, 3), alpha.reshape(th, tw)


def iter_tiles(width, height, tile_size):
    tiles = []
    tid = 0
    for ty in range(0, height, tile_size):
        for tx in range(0, width, tile_size):
            tiles.append((tid, tx, ty, min(tile_size, width - tx), min(tile_size, height - ty)))
            tid += 1
    return tiles


def render_tile(ctx, scene, camera, layers, tile):
    """Render one tile; returns (rgba uint8, owner uint8) for the tile."""
    _, tx0, ty0, tw, th = tile
    acc_rgb, acc_a = _render_wall_tile(ctx, scene, camera, (tx0, ty0, tw, th))
    owner = np.full((th, tw), OWNER_BACKGROUND, dtype=np.uint8)
    owner[acc_a > 0.0] = OWNER_WALL
    acc_rgb = acc_rgb * acc_a[:, :, None]

    for layer in layers:
        src_rgb, src_a, covered = _rasterise_layer_tile(
            ctx, layer, camera, (tx0, ty0, tw, th))
        if not covered.any():
            continue
        acc_rgb = src_rgb + acc_rgb * (1.0 - src_a[:, :, None])
        acc_a = src_a + acc_a * (1.0 - src_a)
        owner[src_a > OWNER_ALPHA_THRESHOLD] = layer["layer_id"]

    acc_rgb = np.clip(acc_rgb, 0.0, 1.0)
    acc_a = np.clip(acc_a, 0.0, 1.0)
    rgba = np.empty((th, tw, 4), dtype=np.uint8)
    rgba[:, :, :3] = np.round(acc_rgb * 255.0).astype(np.uint8)
    rgba[:, :, 3] = np.round(acc_a * 255.0).astype(np.uint8)
    return rgba, owner
