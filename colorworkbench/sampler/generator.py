"""本地确定性样本生成器（固定种子，无网络、无外部 ICC 文件）。

产出：
  samples/images/*.cimg   不同分辨率、8/16 位、单/多通道图像
  samples/profiles/*.json 三套版本的源/目标 ICC 风格配置文件
  samples/manifest.json   样本清单 + 配置解析真值 + 位深声明
  samples/corrupt/*.cimg  用于 E_DECODE 链路的损坏样本
"""

from __future__ import annotations

import json
import os

import numpy as np

from cwb.image_io import encode_image
from cwb.trc import trc_inversion_difference
from cwb.model import TRC

SEED = 20261123

# 同一颜色科学基准，三种版本只是字段表达不同
SRC_MATRIX = [
    0.4360747, 0.3850649, 0.1430804,
    0.2225045, 0.7168786, 0.0606169,
    0.0139322, 0.0971045, 0.7141733,
]
DST_MATRIX = [
    0.4123908, 0.3575843, 0.1804808,
    0.2126390, 0.7151687, 0.0721923,
    0.0193308, 0.1191948, 0.9505322,
]
D65 = [0.95047, 1.0, 1.08883]
D50 = [0.9642, 1.0, 0.8249]


def _rng_image(height: int, width: int, channels: int, depth: int,
               rng: np.random.Generator) -> np.ndarray:
    # 渐变 + 固定随机噪声，保证确定性且跨分辨率结构一致
    dtype = np.uint16 if depth == 16 else np.uint8
    maxv = 65535 if depth == 16 else 255
    yy = np.linspace(0, maxv, height, dtype=np.float64)[:, None]
    xx = np.linspace(0, maxv, width, dtype=np.float64)[None, :]
    base = (yy * 0.5 + xx * 0.5)
    img = np.zeros((height, width, channels), dtype=np.float64)
    for ch in range(channels):
        shift = ch * maxv / max(channels, 2)
        noise = rng.integers(0, max(2, maxv // 16), size=(height, width))
        img[..., ch] = np.clip(base + shift + noise, 0, maxv)
    return np.rint(img).astype(dtype)


def _lut3d(size: int, rng: np.random.Generator) -> dict:
    # 确定性 3D LUT：对角主响应 + 小扰动
    coords = [np.linspace(0, 1, size) for _ in range(3)]
    r, g, b = np.meshgrid(*coords, indexing="ij")
    jitter = rng.random((size, size, size, 3)) * 0.05
    grid = np.stack([r, g, b], axis=-1) + jitter
    grid = np.clip(grid, 0, 1)
    return {"size": size, "data": grid.reshape(-1, 3).tolist()}


def _gamma_table(gamma: float, n: int = 33) -> list[float]:
    x = np.linspace(0, 1, n)
    return [float(v) for v in np.power(x, gamma)]


# ---- 三套版本配置（源/目标各一份，颜色科学基准一致） ----
def _v1(profile_id: str, matrix, wp, default_wp, gamma) -> dict:
    return {
        "profile_version": 1,
        "profile_id": profile_id,
        "color_space": "RGB",
        "red_colorant": [matrix[0], matrix[3], matrix[6]],
        "green_colorant": [matrix[1], matrix[4], matrix[7]],
        "blue_colorant": [matrix[2], matrix[5], matrix[8]],
        "red_trc": {"type": "gamma", "value": gamma},
        "media_white_point": wp,
        "default_white_point": default_wp,
        "default_rendering_intent": 1,
    }


def _v2(profile_id: str, matrix, wp, default_wp, gamma, lut) -> dict:
    return {
        "ver": 2,
        "id": profile_id,
        "cs": "rgb",
        "matrix3x3": matrix,
        "trc": {"mode": "table", "samples": _gamma_table(gamma)},
        # 版本 2：白点字段可选，这里源配置故意省略 wp，仅给声明默认值
        "wp": wp,
        "declared_default_wp": default_wp,
        "intent": "rel",
        "lut3d": lut,
    }


def _v3(profile_id: str, matrix, wp, default_wp, gamma) -> dict:
    return {
        "schemaVersion": "3.0",
        "profileId": profile_id,
        "colorSpace": "rgb",
        "matrix": {"rows": [matrix[0:3], matrix[3:6], matrix[6:9]]},
        "toneResponse": {"kind": "parametric",
                         "type": "gamma", "value": gamma},
        "whitePoint": wp,
        "declaredDefaultWhitePoint": default_wp,
        "renderingIntent": "relative-colorimetric",
    }


def generate(root: str) -> dict:
    rng = np.random.default_rng(SEED)
    img_dir = os.path.join(root, "images")
    prof_dir = os.path.join(root, "profiles")
    corrupt_dir = os.path.join(root, "corrupt")
    for d in (img_dir, prof_dir, corrupt_dir):
        os.makedirs(d, exist_ok=True)

    image_specs = [
        ("tile_16x16_8_rgb", 16, 16, 3, 8),
        ("tile_24x32_8_gray", 24, 32, 1, 8),
        ("tile_32x24_16_rgb", 32, 24, 3, 16),
        ("tile_40x40_16_rgba", 40, 40, 4, 16),
        ("tile_48x32_8_rgba", 48, 32, 2, 8),
        ("tile_64x48_16_gray", 64, 48, 1, 16),
    ]
    images = []
    for name, h, w, c, depth in image_specs:
        arr = _rng_image(h, w, c, depth, rng)
        path = os.path.join(img_dir, name + ".cimg")
        encode_image(path, arr, depth)
        images.append({"name": name, "file": f"images/{name}.cimg",
                       "height": h, "width": w, "channels": c,
                       "depth": depth, "sha256": _arr_hash(arr)})

    # 损坏样本：截断 + 错误魔数
    good = os.path.join(img_dir, image_specs[0][0] + ".cimg")
    blob = open(good, "rb").read()
    open(os.path.join(corrupt_dir, "truncated.cimg"), "wb").write(blob[:40])
    open(os.path.join(corrupt_dir, "badmagic.cimg"), "wb").write(
        b"XXXXIMG0" + blob[8:])

    lut = _lut3d(4, rng)
    profiles = [
        ("src_v1", _v1("src-v1", SRC_MATRIX, D65, D65, 2.2)),
        ("dst_v1", _v1("dst-v1", DST_MATRIX, D50, D50, 2.2)),
        ("src_v2", _v2("src-v2", SRC_MATRIX, None, D65, 2.2, lut)),
        ("dst_v2", _v2("dst-v2", DST_MATRIX, D50, D50, 2.2, None)),
        ("src_v3", _v3("src-v3", SRC_MATRIX, D65, D65, 2.2)),
        ("dst_v3", _v3("dst-v3", DST_MATRIX, D50, D50, 2.2)),
    ]
    prof_manifest = []
    for name, obj in profiles:
        path = os.path.join(prof_dir, name + ".json")
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(obj, fh, indent=2, sort_keys=True)
        prof_manifest.append({
            "name": name, "file": f"profiles/{name}.json",
            "source_version": obj.get("profile_version",
                                      obj.get("ver", obj.get("schemaVersion"))),
            "has_white_point": _has_wp(obj),
            "declared_default_wp": _default_wp(obj),
            "trc_kind": _trc_kind(obj),
            "intent_token": _intent_token(obj),
        })

    # 表格 vs 参数 TRC 反演差异的可测记录（gamma 2.2 基准）
    param = TRC(kind="parametric", curve="gamma", gamma=2.2)
    table = TRC(kind="table", table=tuple(_gamma_table(2.2)))
    trc_diff = trc_inversion_difference(param, table)

    manifest = {
        "seed": SEED,
        "algorithm_version": "cwb-algo-1.0.0",
        "images": images,
        "profiles": prof_manifest,
        "trc_inversion_reference": trc_diff,
        "corrupt": ["corrupt/truncated.cimg", "corrupt/badmagic.cimg"],
        "no_network": True,
        "no_external_icc": True,
    }
    with open(os.path.join(root, "manifest.json"), "w",
              encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)
    return manifest


def _arr_hash(arr: np.ndarray) -> str:
    import hashlib
    return hashlib.sha256(np.ascontiguousarray(arr).tobytes()).hexdigest()


def _has_wp(obj: dict) -> bool:
    return ("media_white_point" in obj and obj["media_white_point"]) or \
           ("wp" in obj and obj["wp"]) or \
           ("whitePoint" in obj and obj["whitePoint"])


def _default_wp(obj: dict):
    return (obj.get("default_white_point")
            or obj.get("declared_default_wp")
            or obj.get("declaredDefaultWhitePoint"))


def _trc_kind(obj: dict) -> str:
    if "red_trc" in obj or (
            "toneResponse" in obj and
            obj["toneResponse"].get("kind") == "parametric"):
        return "parametric"
    if "trc" in obj or "toneResponse" in obj:
        return "table" if obj.get("trc", {}).get("mode") == "table" \
            else "parametric"
    return "unknown"


def _intent_token(obj: dict):
    return (obj.get("default_rendering_intent")
            or obj.get("intent") or obj.get("renderingIntent"))


if __name__ == "__main__":
    import sys
    target = sys.argv[1] if len(sys.argv) > 1 else "samples"
    info = generate(target)
    print(f"generated {len(info['images'])} images, "
          f"{len(info['profiles'])} profiles into {target}")
