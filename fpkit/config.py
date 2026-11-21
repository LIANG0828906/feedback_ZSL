"""Config file loading; budgets/parallelism come only from here."""

import json

DEFAULT_CONFIG = {
    "config_version": "v1.0",
    "scheduler": {
        "parallelism": 4,
        "per_task_mem_budget": 8 * 1024 * 1024,
        "pool_mem_budget": 256 * 1024 * 1024,
    },
    "fingerprint": {
        "frame_size": 2048,
        "hop_size": 512,
        "window": "hann",
        "bands": [[200, 800], [800, 1600], [1600, 3200], [3200, 6400]],
        "peaks_per_band": 2,
        "fan_out": 3,
        "target_dt_min": 1,
        "target_dt_max": 48,
        "time_quant": 4,
        "frame_batch": 256,
    },
    "budget": {
        "degrade_sequence": [
            {"stage": 1, "action": "shrink_frame_batch", "frame_batch": 64},
            {"stage": 2, "action": "shrink_frame_batch", "frame_batch": 16},
            {"stage": 3, "action": "chunk_hash_table", "frame_batch": 16,
             "hash_chunk": 1024},
        ],
    },
    "search": {
        "min_votes": 6,
        "time_tolerance_frames": 2,
    },
}


def load_config(path=None):
    if path is None:
        return json.loads(json.dumps(DEFAULT_CONFIG))
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def save_config(config, path):
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(config, fh, indent=1, sort_keys=True)
        fh.write("\n")
