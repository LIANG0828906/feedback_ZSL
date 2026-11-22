"""运行前样本清单完整性、位深声明与离线依赖检查。"""

from __future__ import annotations

import json
import os

from .errors import CwbError, E_DECODE
from .image_io import probe_depth


def check_samples(root: str) -> dict:
    manifest_path = os.path.join(root, "manifest.json")
    if not os.path.exists(manifest_path):
        raise CwbError(E_DECODE, f"样本清单缺失: {manifest_path}")
    with open(manifest_path, encoding="utf-8") as fh:
        manifest = json.load(fh)

    problems: list[str] = []
    for spec in manifest["images"]:
        path = os.path.join(root, spec["file"])
        if not os.path.exists(path):
            problems.append(f"图像文件缺失: {spec['file']}")
            continue
        try:
            depth = probe_depth(path)
        except CwbError as exc:
            problems.append(str(exc))
            continue
        if depth != spec["depth"]:
            problems.append(
                f"{spec['name']} 位深声明 {spec['depth']} 与实际 {depth} 不符")

    for spec in manifest["profiles"]:
        if not os.path.exists(os.path.join(root, spec["file"])):
            problems.append(f"配置文件缺失: {spec['file']}")

    for rel in manifest.get("corrupt", []):
        if not os.path.exists(os.path.join(root, rel)):
            problems.append(f"损坏样本缺失: {rel}")

    if not manifest.get("no_network") or not manifest.get("no_external_icc"):
        problems.append("清单未声明离线/无外部 ICC 依赖")
    if problems:
        raise CwbError(E_DECODE, "样本完整性检查失败",
                       detail={"problems": problems})
    return {
        "images": len(manifest["images"]),
        "profiles": len(manifest["profiles"]),
        "corrupt": len(manifest.get("corrupt", [])),
        "offline": True,
    }
