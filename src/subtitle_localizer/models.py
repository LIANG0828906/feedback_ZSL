from __future__ import annotations

from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Mapping

from .errors import BusinessError, E_INVALID_INPUT
from .hashing import decimal_string


SCHEMA_ASSET = "subtitle-asset/v1"
SCHEMA_REPORT = "layout-report/v1"
SCHEMA_STATE = "subtitle-layout/v1"


def _int(value: Any, name: str, default: int | None = None) -> int:
    if value is None and default is not None:
        return default
    if isinstance(value, bool) or not isinstance(value, int):
        raise BusinessError(E_INVALID_INPUT, f"{name} must be an integer", {"field": name})
    return value


def _str(value: Any, name: str, default: str | None = None) -> str:
    if value is None and default is not None:
        return default
    if not isinstance(value, str):
        raise BusinessError(E_INVALID_INPUT, f"{name} must be a string", {"field": name})
    return value


@dataclass(frozen=True)
class SentenceInput:
    id: str
    text: str
    start: str
    end: str
    speaker: str
    cps: str | None = None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "end": self.end,
            "id": self.id,
            "speaker": self.speaker,
            "start": self.start,
            "text": self.text,
        }
        if self.cps is not None:
            result["cps"] = self.cps
        return result

    def with_updates(self, updates: Mapping[str, Any]) -> "SentenceInput":
        allowed = {"text", "start", "end", "speaker", "cps"}
        unknown = set(updates) - allowed
        if unknown:
            raise BusinessError(
                E_INVALID_INPUT,
                "unknown sentence fields",
                {"sentence_id": self.id, "fields": sorted(unknown)},
            )
        data = self.to_dict()
        for key, value in updates.items():
            if key == "cps" and value is None:
                data.pop("cps", None)
            elif value is not None:
                data[key] = value
        if "cps" in data:
            try:
                data["cps"] = decimal_string(data["cps"])
            except ValueError as exc:
                raise BusinessError(
                    E_INVALID_INPUT,
                    "sentence cps is invalid",
                    {"field": "cps", "sentence_id": self.id, "value": data["cps"]},
                ) from exc
        return SentenceInput.from_dict(data)

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "SentenceInput":
        if not isinstance(value, Mapping):
            raise BusinessError(E_INVALID_INPUT, "sentence must be an object")
        cps_value = value.get("cps")
        try:
            cps = None if cps_value is None else decimal_string(cps_value)
        except ValueError as exc:
            raise BusinessError(
                E_INVALID_INPUT,
                "sentence cps is invalid",
                {"field": "cps", "sentence_id": value.get("id"), "value": cps_value},
            ) from exc
        return cls(
            id=_str(value.get("id"), "id"),
            text=_str(value.get("text"), "text"),
            start=_str(value.get("start"), "start"),
            end=_str(value.get("end"), "end"),
            speaker=_str(value.get("speaker"), "speaker"),
            cps=cps,
        )


@dataclass(frozen=True)
class DescriptionInput:
    id: str
    after_sentence_id: str
    text: str
    speaker: str
    max_cps: str
    min_duration_ms: int = 400
    priority: int = 50

    def to_dict(self) -> dict[str, Any]:
        return {
            "after_sentence_id": self.after_sentence_id,
            "id": self.id,
            "max_cps": self.max_cps,
            "min_duration_ms": self.min_duration_ms,
            "priority": self.priority,
            "speaker": self.speaker,
            "text": self.text,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any], default_cps: str = "10") -> "DescriptionInput":
        if not isinstance(value, Mapping):
            raise BusinessError(E_INVALID_INPUT, "description must be an object")
        try:
            max_cps = decimal_string(value.get("max_cps", default_cps))
        except ValueError as exc:
            raise BusinessError(
                E_INVALID_INPUT,
                "description max_cps is invalid",
                {"description_id": value.get("id"), "field": "max_cps", "value": value.get("max_cps")},
            ) from exc
        return cls(
            id=_str(value.get("id"), "id"),
            after_sentence_id=_str(value.get("after_sentence_id"), "after_sentence_id"),
            text=_str(value.get("text"), "text"),
            speaker=_str(value.get("speaker"), "speaker"),
            max_cps=max_cps,
            min_duration_ms=_int(value.get("min_duration_ms"), "min_duration_ms", 400),
            priority=_int(value.get("priority"), "priority", 50),
        )


@dataclass(frozen=True)
class LayoutParams:
    default_cps: str = "10"
    min_duration_ms: int = 800
    min_gap_ms: int = 200
    sentence_priority: int = 50
    default_channel: int = 0
    channel_map: Mapping[str, int] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_map": {k: int(v) for k, v in sorted(self.channel_map.items())},
            "default_channel": self.default_channel,
            "default_cps": self.default_cps,
            "min_duration_ms": self.min_duration_ms,
            "min_gap_ms": self.min_gap_ms,
            "sentence_priority": self.sentence_priority,
        }

    def with_updates(self, updates: Mapping[str, Any]) -> "LayoutParams":
        allowed = {
            "default_cps",
            "min_duration_ms",
            "min_gap_ms",
            "sentence_priority",
            "default_channel",
            "channel_map",
        }
        unknown = set(updates) - allowed
        if unknown:
            raise BusinessError(E_INVALID_INPUT, "unknown parameter fields", {"fields": sorted(unknown)})
        data = self.to_dict()
        data.update(updates)
        return LayoutParams.from_dict(data)

    @classmethod
    def from_dict(cls, value: Mapping[str, Any] | None) -> "LayoutParams":
        value = value or {}
        channel_map_raw = value.get("channel_map") or {}
        if not isinstance(channel_map_raw, Mapping):
            raise BusinessError(E_INVALID_INPUT, "channel_map must be an object", {"field": "channel_map"})
        channel_map = {
            str(key): _int(raw, f"channel_map.{key}") for key, raw in channel_map_raw.items()
        }
        try:
            default_cps = decimal_string(value.get("default_cps", "10"))
        except ValueError as exc:
            raise BusinessError(
                E_INVALID_INPUT,
                "default_cps is invalid",
                {"field": "default_cps", "value": value.get("default_cps")},
            ) from exc
        return cls(
            default_cps=default_cps,
            min_duration_ms=_int(value.get("min_duration_ms"), "min_duration_ms", 800),
            min_gap_ms=_int(value.get("min_gap_ms"), "min_gap_ms", 200),
            sentence_priority=_int(value.get("sentence_priority"), "sentence_priority", 50),
            default_channel=_int(value.get("default_channel"), "default_channel", 0),
            channel_map=dict(sorted(channel_map.items())),
        )


@dataclass(frozen=True)
class SubtitleAsset:
    asset_id: str
    sentences: tuple[SentenceInput, ...]
    descriptions: tuple[DescriptionInput, ...]
    parameters: LayoutParams

    def to_dict(self) -> dict[str, Any]:
        return {
            "asset_id": self.asset_id,
            "descriptions": [item.to_dict() for item in self.descriptions],
            "parameters": self.parameters.to_dict(),
            "schema": SCHEMA_ASSET,
            "sentences": [item.to_dict() for item in self.sentences],
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "SubtitleAsset":
        if not isinstance(value, Mapping):
            raise BusinessError(E_INVALID_INPUT, "asset must be an object")
        params = LayoutParams.from_dict(value.get("parameters") or value.get("params"))
        raw_sentences = value.get("sentences")
        raw_descriptions = value.get("descriptions", [])
        if not isinstance(raw_sentences, list):
            raise BusinessError(E_INVALID_INPUT, "sentences must be an array", {"field": "sentences"})
        if not isinstance(raw_descriptions, list):
            raise BusinessError(E_INVALID_INPUT, "descriptions must be an array", {"field": "descriptions"})
        sentences = tuple(SentenceInput.from_dict(item) for item in raw_sentences)
        descriptions = tuple(
            DescriptionInput.from_dict(item, params.default_cps) for item in raw_descriptions
        )
        return cls(
            asset_id=_str(value.get("asset_id"), "asset_id"),
            sentences=sentences,
            descriptions=descriptions,
            parameters=params,
        )

    @classmethod
    def load(cls, path: Path | str) -> "SubtitleAsset":
        import json

        with Path(path).open("r", encoding="utf-8") as handle:
            return cls.from_dict(json.load(handle))

    def replace(self, **changes: Any) -> "SubtitleAsset":
        return replace(self, **changes)

    def sentence_by_id(self, sentence_id: str) -> SentenceInput:
        for sentence in self.sentences:
            if sentence.id == sentence_id:
                return sentence
        raise BusinessError(E_INVALID_INPUT, "unknown sentence", {"sentence_id": sentence_id})

