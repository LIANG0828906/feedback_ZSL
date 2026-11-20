"""End-to-end generation, replay and artifact commit boundaries."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .config import load_config
from .errors import E_IO, E_REPLAY_MISMATCH, E_TRACE_INVALID, WorkbenchError
from .generator import GenerationResult, generate_level
from .models import WorkbenchConfig
from .trace import (
    extract_random_draws,
    load_trace_document,
    trace_config,
    trace_json,
    validate_trace,
)
from .util import atomic_write_text, content_hash


__all__ = [
    "GenerationResult",
    "ReplayResult",
    "generate_from_config",
    "generate_from_document",
    "replay_trace",
    "write_generation_artifacts",
]


@dataclass
class ReplayResult:
    generation: GenerationResult
    source_trace_hash: str
    replay_trace_hash: str
    layout_hash_verified: bool
    attribution_hash_verified: bool
    event_chain_verified: bool

    def summary(self) -> dict[str, Any]:
        payload = self.generation.summary()
        payload.update(
            {
                "replayed": True,
                "source_trace_hash": self.source_trace_hash,
                "replay_trace_hash": self.replay_trace_hash,
                "layout_hash_verified": self.layout_hash_verified,
                "attribution_hash_verified": self.attribution_hash_verified,
                "event_chain_verified": self.event_chain_verified,
            }
        )
        return payload


def generate_from_document(
    document: dict[str, Any],
    *,
    cancel_event: Any | None = None,
) -> GenerationResult:
    from .config import parse_config_document

    config = parse_config_document(document)
    return generate_from_config(config, cancel_event=cancel_event)


def generate_from_config(
    config: WorkbenchConfig,
    *,
    cancel_event: Any | None = None,
) -> GenerationResult:
    return generate_level(config, cancel_event=cancel_event)


def generate_from_path(
    path: str | Path,
    *,
    injection: str | None = None,
    cancel_event: Any | None = None,
) -> GenerationResult:
    config = load_config(path, injection=injection)
    return generate_from_config(config, cancel_event=cancel_event)


def replay_trace(
    trace: str | Path | dict[str, Any],
    *,
    cancel_event: Any | None = None,
) -> ReplayResult:
    if isinstance(trace, dict):
        document = load_trace_document(document=trace)
    else:
        document = load_trace_document(path=trace)
    config = trace_config(document)
    draws = extract_random_draws(document)
    replay = generate_level(
        config,
        cancel_event=cancel_event,
        replay_draws=draws,
    )
    replay_draw_count = len(
        [
            event
            for event in replay.trace["events"]
            if event.get("kind") == "random_draw"
        ]
    )
    if replay_draw_count != len(draws):
        raise WorkbenchError(
            E_TRACE_INVALID,
            "trace contains unconsumed random draws",
            details={"recorded": len(draws), "consumed": replay_draw_count},
        )
    meta = document["meta"]
    layout_verified = replay.layout_hash == meta.get("layout_hash")
    attribution_verified = replay.attribution_hash == meta.get("attribution_hash")
    original_event_hashes = [event.get("hash") for event in document["events"]]
    replay_event_hashes = [event.get("hash") for event in replay.trace["events"]]
    event_chain_verified = original_event_hashes == replay_event_hashes
    if not layout_verified or not attribution_verified:
        raise WorkbenchError(
            E_REPLAY_MISMATCH,
            "trace replay produced a different layout or attribution record",
            details={
                "expected_layout_hash": meta.get("layout_hash"),
                "actual_layout_hash": replay.layout_hash,
                "expected_attribution_hash": meta.get("attribution_hash"),
                "actual_attribution_hash": replay.attribution_hash,
            },
        )
    if not event_chain_verified or replay.trace["meta"].get("trace_hash") != meta.get("trace_hash"):
        raise WorkbenchError(
            E_REPLAY_MISMATCH,
            "trace replay diverged from the recorded hash chain",
            details={
                "expected_trace_hash": meta.get("trace_hash"),
                "actual_trace_hash": replay.trace["meta"].get("trace_hash"),
            },
        )
    return ReplayResult(
        generation=replay,
        source_trace_hash=str(meta.get("trace_hash")),
        replay_trace_hash=str(replay.trace["meta"].get("trace_hash")),
        layout_hash_verified=layout_verified,
        attribution_hash_verified=attribution_verified,
        event_chain_verified=event_chain_verified,
    )


def write_generation_artifacts(
    result: GenerationResult,
    output_dir: str | Path,
    *,
    level_name: str = "level.json",
    trace_name: str = "trace.json",
) -> dict[str, str]:
    """Commit level and trace together after all validation succeeds."""

    directory = Path(output_dir)
    directory.mkdir(parents=True, exist_ok=True)
    level_path = directory / level_name
    trace_path = directory / trace_name
    level_text = json.dumps(result.level.to_dict(), ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    trace_text = trace_json(result.trace)
    level_tmp = directory / f".{level_name}.tmp"
    trace_tmp = directory / f".{trace_name}.tmp"
    try:
        with level_tmp.open("w", encoding="utf-8", newline="\n") as handle:
            handle.write(level_text)
            handle.flush()
        with trace_tmp.open("w", encoding="utf-8", newline="\n") as handle:
            handle.write(trace_text)
            handle.flush()
        level_tmp.replace(level_path)
        trace_tmp.replace(trace_path)
    except OSError as exc:
        raise WorkbenchError(E_IO, f"could not commit artifacts: {exc}") from exc
    finally:
        level_tmp.unlink(missing_ok=True)
        trace_tmp.unlink(missing_ok=True)
    return {"level": str(level_path), "trace": str(trace_path)}


def trace_layout_hash(document: dict[str, Any]) -> str:
    validate_trace(document)
    return str(document["meta"]["layout_hash"])
