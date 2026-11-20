from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

MODEL_VERSION = "br-econ-v1"
BASELINE_VERSION = 1
SNAPSHOT_VERSION = 1

# Fixed model genesis. A chain is fully determined by this value and the
# per-step serialized state; no clock, filename, or filesystem metadata enters.
GENESIS_CHAIN_HASH = (
    "9a2a09380bac1313c76d715f328ebce4fcde9f6598c76fd609782e692f735950"
)


def compact_json(value: Any) -> str:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False)


def sha256_hex(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def state_hash(state: dict[str, Any]) -> str:
    return sha256_hex(compact_json(state))


def chain_hash(previous: str, content_hash: str) -> str:
    return sha256_hex(previous + "\n" + content_hash)


def read_json(path: str | Path) -> Any:
    with Path(path).open("r", encoding="utf-8") as handle:
        return json.load(handle)


def canonical_file_digest(path: str | Path) -> str:
    return sha256_hex(compact_json(read_json(path)))

