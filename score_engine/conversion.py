from __future__ import annotations

import json
from dataclasses import dataclass, replace
from fractions import Fraction
from pathlib import Path
from typing import Any

from .errors import version_loss
from .model import ExpressionAnchor, Score
from .parser import _normalise_tempo, parse_score as _parse_score
from .util import fraction_text, rounded_float
from .validation import validate_score


@dataclass
class ConversionResult:
    score: Score
    target_version: int
    payload: dict[str, Any]
    losses: list[dict[str, Any]]

    @property
    def loss_count(self) -> int:
        return len(self.losses)

    def report(self) -> dict[str, Any]:
        return {
            "code": "E_VERSION_LOSS" if self.losses else "OK",
            "source_version": self.score.source_version,
            "target_version": self.target_version,
            "derived": dict(sorted(self.score.provenance.items())),
            "loss_count": len(self.losses),
            "losses": self.losses,
        }


def parse_score(source: str | Path | dict[str, Any]) -> Score:
    return _parse_score(source)


def _losses_for_target(score: Score, target_version: int) -> list[dict[str, Any]]:
    losses: list[dict[str, Any]] = []

    def add(note_id: str, field: str, part_id: str | None = None, detail: str | None = None) -> None:
        record = {
            "code": "E_VERSION_LOSS",
            "note_id": note_id,
            "field": field,
        }
        if part_id is not None:
            record["part_id"] = part_id
        if detail is not None:
            record["detail"] = detail
        if record not in losses:
            losses.append(record)

    if target_version <= 2:
        if score.expression:
            for part in score.parts:
                relevant = [
                    anchor
                    for anchor in score.expression
                    if anchor.part_id is None or anchor.part_id == part.id
                ]
                if not relevant:
                    continue
                for note in part.notes:
                    add(note.id, "expression", part.id, "v2/v1 无法表达表情曲线锚点")
        for part in score.parts:
            for note in part.notes:
                add(note.id, "part.grouping", part.id, "旧版轨道不保留 v3 声部分组语义")
                add(note.id, "pitch_range", part.id, "旧版不声明乐器音域")
            if len(score.tempo_segments) > 1:
                for note in part.notes:
                    add(note.id, "tempo_segments", part.id, "旧版只表达单一基础速度")
    if target_version == 1:
        if len(score.time_signatures) > 1:
            for part in score.parts:
                for note in part.notes:
                    add(note.id, "time_signatures", part.id, "v1 只记录单一拍号")
        for part in score.parts:
            for note in part.notes:
                add(note.id, "velocity", part.id, "v1 仅记录音高/起始拍/时值")
                add(note.id, "articulation", part.id, "v1 仅记录音高/起始拍/时值")
                if part.pedals:
                    add(note.id, "pedals", part.id, "v1 无法表达延音踏板区间")
    return losses


def _v3_payload(score: Score) -> dict[str, Any]:
    payload = score.to_internal_dict()
    payload["source_version"] = 3
    payload["format_version"] = "3.0"
    return payload


def _v2_payload(score: Score) -> dict[str, Any]:
    offset = score.start_offset
    first_bpm = score.tempo_segments[0].bpm
    tempo_payload: dict[str, Any] = {
        "bpm_number": fraction_text(first_bpm),
        "bpm": fraction_text(first_bpm),
    }
    tracks: list[dict[str, Any]] = []
    for part in score.parts:
        tracks.append(
            {
                "id": part.id,
                "name": part.name,
                "instrument": part.instrument,
                "notes": [
                    {
                        "id": note.id,
                        "pitch": note.pitch,
                        "start_beat": fraction_text(note.start - offset),
                        "duration_beats": fraction_text(note.duration),
                        "velocity": note.velocity,
                        "articulation": note.articulation,
                    }
                    for note in part.notes
                ],
                "pedal_spans": [
                    {
                        "id": pedal.id,
                        "start_beat": fraction_text(pedal.start - offset),
                        "end_beat": fraction_text(pedal.end - offset),
                        "value": fraction_text(pedal.value),
                    }
                    for pedal in part.pedals
                ],
            }
        )
    return {
        "format_version": "2.0",
        "title": score.title,
        "composer": score.composer,
        "origin_offset_beats": fraction_text(offset),
        "grid_resolution": fraction_text(score.grid_resolution),
        "time_unit": "quarter",
        "tempo_bpm": fraction_text(first_bpm),
        "tempo": tempo_payload,
        "meter_changes": [
            {
                "id": signature.id,
                "beat": fraction_text(signature.start),
                "numerator": signature.numerator,
                "denominator": signature.denominator,
            }
            for signature in score.time_signatures
        ],
        "tracks": tracks,
    }


def _v1_payload(score: Score) -> dict[str, Any]:
    bpm = score.tempo_segments[0].bpm
    offset_seconds = score.start_offset * 60 / bpm
    factor = Fraction(4)
    part = score.parts[0]
    return {
        "format_version": 1,
        "title": score.title,
        "composer": score.composer,
        "time_signature": {
            "numerator": score.time_signatures[0].numerator,
            "denominator": score.time_signatures[0].denominator,
        },
        "tempo_bpm": fraction_text(bpm),
        "start_offset_seconds": fraction_text(offset_seconds),
        "duration_unit": "whole_note",
        "grid_resolution": fraction_text(score.grid_resolution),
        "notes": [
            {
                "id": note.id,
                "pitch": note.pitch,
                "start": fraction_text((note.start - score.start_offset) / factor),
                "duration": fraction_text(note.duration / factor),
            }
            for note in part.notes
        ],
    }


def export_score(
    score: Score,
    target_version: int,
    *,
    allow_loss: bool = False,
) -> ConversionResult:
    if target_version not in {1, 2, 3}:
        from .errors import score_invalid

        raise score_invalid(
            f"不支持的目标版本: {target_version}",
            note_id="-",
            field="target_version",
        )
    validate_score(score)
    losses = _losses_for_target(score, target_version)
    if losses and not allow_loss:
        raise version_loss(
            f"导出到 v{target_version} 会丢失 {len(losses)} 个字段，未写入文件",
            losses,
        )
    if target_version == 3:
        payload = _v3_payload(score)
    elif target_version == 2:
        payload = _v2_payload(score)
    else:
        payload = _v1_payload(score)
    return ConversionResult(
        score=score,
        target_version=target_version,
        payload=payload,
        losses=losses,
    )


def convert_score(
    source: str | Path | dict[str, Any],
    target_version: int,
    *,
    allow_loss: bool = False,
) -> ConversionResult:
    score = parse_score(source)
    return export_score(score, target_version, allow_loss=allow_loss)


def export_to_file(
    score: Score,
    target_version: int,
    path: str | Path,
    *,
    allow_loss: bool = False,
) -> ConversionResult:
    from .util import atomic_write_json

    result = export_score(score, target_version, allow_loss=allow_loss)
    atomic_write_json(Path(path), result.payload)
    return result


def round_trip_payload(payload: dict[str, Any], target_version: int) -> ConversionResult:
    score = parse_score(payload)
    return export_score(score, target_version, allow_loss=True)

