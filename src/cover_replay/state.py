"""Mutable step state plus deterministic snapshot serialization."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .fixed import format_units, quantize_units
from .model import CoverSpec, Revoke


@dataclass
class CoverState:
    cover_id: str
    damage_units: int = 0
    direct_damage_units: int = 0
    propagated_damage_units: int = 0
    predicted_damage_units: int = 0
    crack_segments: int = 0
    quantization_operations: int = 0

    def remaining_units(self, cover: CoverSpec, decimals: int, mode: str) -> int:
        capacity = quantize_units(cover.max_durability, decimals, mode)
        return max(0, capacity - self.damage_units)

    def predicted_remaining_units(
        self,
        cover: CoverSpec,
        decimals: int,
        mode: str,
    ) -> int:
        capacity = quantize_units(cover.max_durability, decimals, mode)
        return capacity - self.predicted_damage_units


@dataclass
class LedgerEntry:
    shot_key: str
    player_id: str
    client_seq: int
    cover_id: str
    effective_step: int
    arrival_seq: int
    predicted_units: int
    source_index: int
    status: str = "pending"
    settled_step: int | None = None
    authoritative_units: int = 0
    predicted_after_units: int | None = None
    authoritative_after_units: int | None = None


@dataclass
class CompensationRecord:
    shot_key: str
    cover_id: str
    revoke_step: int
    original_settled_step: int
    restoration_units: int
    authoritative_unchanged_units: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "shot_key": self.shot_key,
            "cover_id": self.cover_id,
            "revoke_step": self.revoke_step,
            "original_settled_step": self.original_settled_step,
            "restoration_units": self.restoration_units,
            "authoritative_unchanged_units": self.authoritative_unchanged_units,
        }


@dataclass
class ReplayState:
    step: int
    covers: dict[str, CoverState]
    ledger: dict[str, LedgerEntry] = field(default_factory=dict)
    compensations: list[CompensationRecord] = field(default_factory=list)
    revocation_log: list[dict[str, Any]] = field(default_factory=list)
    pending_injected_revokes: list[Revoke] = field(default_factory=list)
    settled_shot_keys: list[str] = field(default_factory=list)
    buffer_watermark_bytes: int = 0
    last_schedule: dict[str, Any] = field(default_factory=dict)
    last_crack_events: list[dict[str, Any]] = field(default_factory=list)
    last_predicted_ledger: list[dict[str, Any]] = field(default_factory=list)
    chain_hash: str = ""
    state_hash: str = ""

    def to_payload(self) -> dict[str, Any]:
        cover_payload = []
        for cover in sorted(self.covers.values(), key=lambda item: item.cover_id):
            cover_payload.append(
                {
                    "cover_id": cover.cover_id,
                    "damage_units": cover.damage_units,
                    "direct_damage_units": cover.direct_damage_units,
                    "propagated_damage_units": cover.propagated_damage_units,
                    "predicted_damage_units": cover.predicted_damage_units,
                    "crack_segments": cover.crack_segments,
                    "quantization_operations": cover.quantization_operations,
                }
            )
        ledger_payload = []
        for key in sorted(self.ledger):
            entry = self.ledger[key]
            ledger_payload.append(
                {
                    "shot_key": entry.shot_key,
                    "player_id": entry.player_id,
                    "client_seq": entry.client_seq,
                    "cover_id": entry.cover_id,
                    "effective_step": entry.effective_step,
                    "arrival_seq": entry.arrival_seq,
                    "predicted_units": entry.predicted_units,
                    "source_index": entry.source_index,
                    "status": entry.status,
                    "settled_step": entry.settled_step,
                    "authoritative_units": entry.authoritative_units,
                    "predicted_after_units": entry.predicted_after_units,
                    "authoritative_after_units": entry.authoritative_after_units,
                }
            )
        return {
            "step": self.step,
            "covers": cover_payload,
            "ledger": ledger_payload,
            "compensations": [record.to_dict() for record in self.compensations],
            "revocation_log": self.revocation_log,
            "pending_revokes": [
                {
                    "player_id": revoke.player_id,
                    "client_seq": revoke.client_seq,
                    "arrival_seq": revoke.arrival_seq,
                    "effective_step": revoke.effective_step,
                    "source": revoke.source,
                }
                for revoke in sorted(
                    self.pending_injected_revokes,
                    key=lambda item: (
                        item.effective_step,
                        item.arrival_seq,
                        item.player_id,
                        item.client_seq,
                        item.index,
                    ),
                )
            ],
            "settled_shot_keys": sorted(self.settled_shot_keys),
            "buffer_watermark_bytes": self.buffer_watermark_bytes,
            "last_schedule": self.last_schedule,
            "last_crack_events": self.last_crack_events,
            "last_predicted_ledger": self.last_predicted_ledger,
        }

    @classmethod
    def from_payload(cls, payload: dict[str, Any]) -> ReplayState:
        covers: dict[str, CoverState] = {}
        for raw_cover in payload["covers"]:
            cover = CoverState(
                cover_id=raw_cover["cover_id"],
                damage_units=raw_cover["damage_units"],
                direct_damage_units=raw_cover["direct_damage_units"],
                propagated_damage_units=raw_cover["propagated_damage_units"],
                predicted_damage_units=raw_cover["predicted_damage_units"],
                crack_segments=raw_cover["crack_segments"],
                quantization_operations=raw_cover["quantization_operations"],
            )
            covers[cover.cover_id] = cover
        ledger: dict[str, LedgerEntry] = {}
        for raw_entry in payload["ledger"]:
            entry = LedgerEntry(
                shot_key=raw_entry["shot_key"],
                player_id=raw_entry["player_id"],
                client_seq=raw_entry["client_seq"],
                cover_id=raw_entry["cover_id"],
                effective_step=raw_entry["effective_step"],
                arrival_seq=raw_entry["arrival_seq"],
                predicted_units=raw_entry["predicted_units"],
                source_index=raw_entry["source_index"],
                status=raw_entry["status"],
                settled_step=raw_entry["settled_step"],
                authoritative_units=raw_entry["authoritative_units"],
                predicted_after_units=raw_entry["predicted_after_units"],
                authoritative_after_units=raw_entry["authoritative_after_units"],
            )
            ledger[entry.shot_key] = entry
        compensations = [
            CompensationRecord(
                shot_key=raw["shot_key"],
                cover_id=raw["cover_id"],
                revoke_step=raw["revoke_step"],
                original_settled_step=raw["original_settled_step"],
                restoration_units=raw["restoration_units"],
                authoritative_unchanged_units=raw["authoritative_unchanged_units"],
            )
            for raw in payload["compensations"]
        ]
        pending = [
            Revoke(
                index=index,
                player_id=raw["player_id"],
                client_seq=raw["client_seq"],
                arrival_seq=raw["arrival_seq"],
                effective_step=raw["effective_step"],
                source=raw.get("source", "injected"),
            )
            for index, raw in enumerate(payload.get("pending_revokes", []))
        ]
        return cls(
            step=payload["step"],
            covers=covers,
            ledger=ledger,
            compensations=compensations,
            revocation_log=list(payload.get("revocation_log", [])),
            pending_injected_revokes=pending,
            settled_shot_keys=list(payload.get("settled_shot_keys", [])),
            buffer_watermark_bytes=payload.get("buffer_watermark_bytes", 0),
            last_schedule=payload.get("last_schedule", {}),
            last_crack_events=payload.get("last_crack_events", []),
            last_predicted_ledger=payload.get("last_predicted_ledger", []),
        )

    def display_ledger(
        self,
        covers: tuple[CoverSpec, ...],
        decimals: int,
        quantization_mode: str,
    ) -> list[dict[str, Any]]:
        return [
            {
                "cover_id": cover.cover_id,
                "predicted_remaining": format_units(
                    self.covers[cover.cover_id].predicted_remaining_units(
                        cover,
                        decimals,
                        quantization_mode,
                    ),
                    decimals,
                ),
                "authoritative_remaining": format_units(
                    self.covers[cover.cover_id].remaining_units(
                        cover,
                        decimals,
                        quantization_mode,
                    ),
                    decimals,
                ),
            }
            for cover in covers
        ]


def initial_state(covers: tuple[CoverSpec, ...]) -> ReplayState:
    return ReplayState(
        step=-1,
        covers={cover.cover_id: CoverState(cover_id=cover.cover_id) for cover in covers},
    )

