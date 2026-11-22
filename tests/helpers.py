from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

from scworkbench.checkpoint import CheckpointManager
from scworkbench.models import SampleSession
from scworkbench.negotiation import negotiate
from scworkbench.samplegen import build_sample

ROOT = Path(__file__).resolve().parents[1]
BASE_DATA: dict[str, Any] = build_sample()


def sample_data() -> dict[str, Any]:
    return copy.deepcopy(BASE_DATA)


def write_sample(directory: str | Path, data: dict[str, Any] | None = None) -> Path:
    path = Path(directory) / "sample_session.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(data or sample_data(), ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    return path


def manager_for(
    directory: str | Path,
    sample: SampleSession,
    content_hash: str | None = None,
    capabilities_hash: str | None = None,
    protocol_version: str | None = None,
) -> CheckpointManager:
    result = negotiate(sample)
    return CheckpointManager(
        directory,
        sample.session_id,
        content_hash or sample.content_hash,
        capabilities_hash or result.capabilities_hash,
        protocol_version or sample.protocol_version,
    )


def code_set(result: Any) -> set[str]:
    codes = {code for frame in result.frames for code in frame.codes}
    codes.update(code for item in result.inputs for code in item.codes)
    codes.update(
        event.code for event in result.phase_events if event.code is not None
    )
    return codes
