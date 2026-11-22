from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from tickarena import JsonlEventSource, make_initial_state
from tickarena.generator import write_generated_match
from tickarena.numeric import canonical_bytes


def write_match(
    directory: str | Path,
    *,
    seed: int = 42,
    players: int = 4,
    ticks: int = 300,
    density: str = "1.25",
    win_score: int = 1_000_000,
) -> tuple[dict[str, Any], Path]:
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    initial = make_initial_state(
        seed=seed,
        players=players,
        tick_count=ticks,
        win_score=win_score,
    )
    path = directory / "events.jsonl"
    written = write_generated_match(path, initial, event_density=density)
    source = JsonlEventSource(path)
    try:
        assert source.fingerprint == written
    finally:
        source.close()
    return initial, path


def rewrite_replay(
    path: str | Path,
    transform: Any,
) -> None:
    """Tamper semantically while preserving a valid framing/hash envelope."""

    replay_path = Path(path)
    lines = replay_path.read_bytes().splitlines(keepends=True)
    header = json.loads(lines[0])
    body: list[dict[str, Any]] = []
    footer: dict[str, Any] | None = None
    for line in lines[1:]:
        record = json.loads(line)
        if record.get("kind") == "footer":
            footer = record
        else:
            body.append(record)
    assert footer is not None
    replacement = transform(body)
    if replacement is not None:
        body = replacement
    hasher = hashlib.sha256()
    output = [canonical_bytes(header) + b"\n"]
    for record in body:
        line = canonical_bytes(record) + b"\n"
        hasher.update(line)
        output.append(line)
    footer["records"] = len(body)
    footer["body_sha256"] = hasher.hexdigest()
    output.append(canonical_bytes(footer) + b"\n")
    replay_path.write_bytes(b"".join(output))

