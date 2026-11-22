"""Bundle loading facade."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import LCFTError
from .timebase import NormalizedSubtitle, TimebaseResult, unify_timebase


@dataclass(frozen=True)
class LoadedBundle:
    path: Path
    raw: dict[str, Any]
    normalized: TimebaseResult

    def sentences(self, language: str) -> tuple[NormalizedSubtitle, ...]:
        try:
            return self.normalized.tracks[language]
        except KeyError as exc:
            raise LCFTError(
                "E_INPUT_INVALID",
                "unknown language track",
                field="tracks",
                details={"language": language},
            ) from exc

    @property
    def languages(self) -> list[str]:
        return sorted(self.normalized.tracks)


def load_bundle(path: str | Path) -> LoadedBundle:
    target = Path(path)
    try:
        with open(target, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise LCFTError(
            "E_INPUT_INVALID",
            "cannot load bundle JSON",
            field="bundle",
            details={"path": str(target), "reason": str(exc)},
        ) from exc
    if not isinstance(raw, dict):
        raise LCFTError(
            "E_INPUT_INVALID", "bundle root must be an object", field="bundle"
        )
    normalized = unify_timebase(raw, base_dir=target.parent)
    return LoadedBundle(path=target, raw=raw, normalized=normalized)

