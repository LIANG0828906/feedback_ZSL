"""声道级结果缓存：内容哈希寻址，npz 载荷 + JSON 元数据。"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

import numpy as np

PIPELINE_VERSION = "1.0.0"


def channel_cache_key(input_ch_hash: str, channel_param_hash: str) -> str:
    blob = json.dumps(
        {
            "input": input_ch_hash,
            "params": channel_param_hash,
            "version": PIPELINE_VERSION,
        },
        sort_keys=True,
    )
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


class ResultCache:
    def __init__(self, cache_dir):
        self.dir = Path(cache_dir)
        self.dir.mkdir(parents=True, exist_ok=True)

    def _paths(self, key: str):
        return self.dir / f"{key}.npz", self.dir / f"{key}.json"

    def get(self, key: str):
        npz_path, meta_path = self._paths(key)
        if not npz_path.exists() or not meta_path.exists():
            return None
        with open(meta_path, "r", encoding="utf-8") as fh:
            meta = json.load(fh)
        if meta.get("key") != key:
            return None
        with np.load(npz_path) as zf:
            enhanced = zf["enhanced"].copy()
        actual = hashlib.sha256(enhanced.tobytes()).hexdigest()
        if actual != meta.get("enhanced_sha256"):
            return None
        return enhanced, meta

    def put(self, key: str, enhanced: np.ndarray, meta: dict) -> None:
        npz_path, meta_path = self._paths(key)
        tmp_npz = self.dir / f"{key}.npz.tmp"
        with open(tmp_npz, "wb") as fh:
            np.savez_compressed(fh, enhanced=enhanced)
        os.replace(tmp_npz, npz_path)
        full_meta = dict(meta)
        full_meta["key"] = key
        full_meta["enhanced_sha256"] = hashlib.sha256(enhanced.tobytes()).hexdigest()
        tmp_meta = self.dir / f"{key}.json.tmp"
        with open(tmp_meta, "w", encoding="utf-8", newline="\n") as fh:
            json.dump(full_meta, fh, sort_keys=True, ensure_ascii=False)
        os.replace(tmp_meta, meta_path)
