#!/usr/bin/env python3
"""一次性构建本地离线 wheelhouse。

仅在准备环境时使用网络；运行期安装通过 `uv pip install --offline` 完成，
不依赖任何在线源。抓取目标固定为当前解释器 (cp314 / x86_64 / linux) 兼容的 wheel。
"""
from __future__ import annotations

import json
import sys
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
WHEELHOUSE = ROOT / "wheelhouse"

# 固定版本，保证离线源可复现。
PINNED = {
    "numpy": "2.5.5",
    "PyQt5": "5.15.11",
    "PyQt5-Qt5": "5.15.19",
    "PyQt5-sip": "12.20.1",
}


def candidate_files(pkg: str, version: str) -> list[dict]:
    url = f"https://pypi.org/pypi/{pkg}/{version}/json"
    with urllib.request.urlopen(url, timeout=30) as resp:
        payload = json.load(resp)
    wanted = []
    for item in payload["urls"]:
        name = item["filename"].lower()
        if not name.endswith(".whl"):
            continue
        if "linux" not in name or "x86_64" not in name:
            continue
        # 仅保留 glibc (manylinux) 标准解释器 ABI，跳过 musllinux 与自由线程 (cp314t)。
        if "musllinux" in name or "cp314t" in name or "cp315t" in name:
            continue
        # 纯 python wheel 直接接受；二进制 wheel 需匹配解释器标签。
        if "py3-none" in name or "cp314" in name or "abi3" in name:
            wanted.append(item)
    if not wanted:
        raise RuntimeError(f"no compatible wheel for {pkg}=={version}")
    return wanted


def download(item: dict) -> Path:
    WHEELHOUSE.mkdir(parents=True, exist_ok=True)
    dest = WHEELHOUSE / item["filename"]
    if dest.exists() and dest.stat().st_size == item["size"]:
        print(f"skip (cached): {dest.name}")
        return dest
    print(f"download: {item['url']}")
    with urllib.request.urlopen(item["url"], timeout=120) as resp:
        dest.write_bytes(resp.read())
    if dest.stat().st_size != item["size"]:
        raise RuntimeError(f"size mismatch for {dest.name}")
    return dest


def main() -> int:
    for pkg, version in PINNED.items():
        for item in candidate_files(pkg, version):
            download(item)
    print(f"\nwheelhouse ready at {WHEELHOUSE}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
