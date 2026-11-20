"""Loss-aware v1/v2 -> v3 fingerprint record migration."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .config import CHANNEL_MERGE_STRATEGIES
from .errors import BizError, E_MIGRATE_LOSS
from .records import FingerprintRecord, RECORD_SCHEMA, _seal_record, parse_record
from .util import content_hash, hash_label


@dataclass(frozen=True)
class MigrationContext:
    channel_merge: str
    compression: dict[str, Any]

    def validate(self) -> None:
        if self.channel_merge not in CHANNEL_MERGE_STRATEGIES:
            raise BizError(
                E_MIGRATE_LOSS,
                "migration requires an explicit channel merge marker",
                {"field": "channel_merge", "value": self.channel_merge},
            )
        if not isinstance(self.compression, dict):
            raise BizError(
                E_MIGRATE_LOSS,
                "migration requires explicit compression metadata",
                {"field": "compression"},
            )
        for field_name, expected in (("codec", str), ("bit_depth", int), ("lossless", bool)):
            if field_name not in self.compression or not isinstance(self.compression[field_name], expected):
                raise BizError(
                    E_MIGRATE_LOSS,
                    "migration compression metadata is incomplete",
                    {"field": f"compression.{field_name}"},
                )


@dataclass
class MigrationOutcome:
    source_record_id: str
    source_version: int
    target_record: FingerprintRecord | None
    field_mapping: list[dict[str, str]]
    lost_fields: list[dict[str, str]]

    @property
    def ok(self) -> bool:
        return not self.lost_fields and self.target_record is not None

    def as_dict(self) -> dict[str, Any]:
        return {
            "source_record_id": self.source_record_id,
            "source_version": self.source_version,
            "target_version": 3,
            "target_record_id": self.target_record.record_id if self.target_record else None,
            "target_checksum": self.target_record.checksum if self.target_record else None,
            "field_mapping": self.field_mapping,
            "lost_fields": self.lost_fields,
        }


@dataclass
class MigrationReport:
    outcomes: list[MigrationOutcome]

    @property
    def lost_fields(self) -> list[dict[str, str]]:
        return [item for outcome in self.outcomes for item in outcome.lost_fields]

    @property
    def affected_records(self) -> list[str]:
        return sorted({item["record_id"] for item in self.lost_fields})

    def as_dict(self) -> dict[str, Any]:
        mapping_by_version: dict[str, list[dict[str, str]]] = {}
        for outcome in self.outcomes:
            mapping_by_version.setdefault(f"v{outcome.source_version}", outcome.field_mapping)
        return {
            "ok": not self.lost_fields,
            "target_version": 3,
            "mapping_table": mapping_by_version,
            "lost_fields": self.lost_fields,
            "affected_records": self.affected_records,
            "records": [outcome.as_dict() for outcome in self.outcomes],
        }


def _mapping(source: str, target: str, transform: str, note: str) -> dict[str, str]:
    return {"source": source, "target": target, "transform": transform, "note": note}


def _field_mapping(version: int) -> list[dict[str, str]]:
    if version == 1:
        return [
            _mapping("v1.payload.peak_pairs", "v3.payload.peak_pairs", "preserve", "peak bins and frame numbers retained"),
            _mapping("v1.payload.frame_number", "v3.payload.frame_number", "preserve", "segment anchor frame retained"),
            _mapping("v1.payload.frame_number", "v3.payload.origin_time_quantization", "preserve", "original frame quantization basis retained"),
            _mapping("v1.payload.peak_pairs", "v3.payload.segment_hashes", "derive", "deterministic v1-pair-raw hashes derived from stored pairs/frames"),
            _mapping("v1.payload.peak_pairs", "v3.payload.segment_hash", "derive", "content hash of stored pairs and derived hashes"),
            _mapping("<migration-context>", "v3.payload.channel_merge", "explicit-context", "v1 did not record channel merge; context is marked in mapping"),
            _mapping("<migration-context>", "v3.payload.compression", "explicit-context", "v1 did not record compression; context is marked in mapping"),
        ]
    return [
        _mapping("v2.payload.peak_pairs", "v3.payload.peak_pairs", "preserve", "peak bins and frame numbers retained"),
        _mapping("v2.payload.segment_hashes", "v3.payload.segment_hashes", "preserve", "hashes retained byte-for-byte at field level"),
        _mapping("v2.payload.segment_hash", "v3.payload.segment_hash", "preserve", "segment content hash retained"),
        _mapping("v2.payload.time_quant_bits", "v3.payload.time_quant_bits", "preserve", "millisecond quantization width retained"),
        _mapping("v2.payload.time_quant_bits", "v3.payload.origin_time_quantization", "preserve", "original quantization basis recorded in origin"),
        _mapping("v2.payload.band_layout", "v3.payload.band_layout", "preserve", "frequency band edges retained"),
        _mapping("<migration-context>", "v3.payload.channel_merge", "explicit-context", "v2 did not record channel merge; context is marked in mapping"),
        _mapping("<migration-context>", "v3.payload.compression", "explicit-context", "v2 did not record compression; context is marked in mapping"),
    ]


def migrate_record(record: FingerprintRecord, context: MigrationContext) -> MigrationOutcome:
    context.validate()
    if record.version == 3:
        return MigrationOutcome(
            record.record_id,
            3,
            record,
            [_mapping("v3", "v3", "identity", "already target version")],
            [],
        )
    if record.version not in (1, 2):
        raise BizError(
            E_MIGRATE_LOSS,
            "only v1 and v2 records can be migrated",
            {"record_id": record.record_id, "field": "version", "value": record.version},
        )

    lost: list[dict[str, str]] = []
    for field_name, value in record.check_extra_fields().items():
        lost.append(
            {
                "record_id": record.record_id,
                "field": field_name,
                "reason": "field has no v3 mapping and would be silently discarded",
                "value_sha256": content_hash(value),
            }
        )
    mapping = _field_mapping(record.version)
    mapping.append(
        _mapping(
            f"v{record.version}.checksum",
            "v3.origin_checksum",
            "preserve",
            "source checksum retained for provenance",
        )
    )

    if record.version == 1:
        time_quant_bits = None
        time_quant_unit = "frame"
        band_layout: list[int] = []
        origin_time = {
            "kind": "frame",
            "frame_number": record.frame_number,
            "sample_rate": record.sample_rate,
            "hop_length": record.hop_length,
        }
        segment_hashes = [hit.as_dict() for hit in record.hashes]
    else:
        time_quant_bits = record.payload["time_quant_bits"]
        time_quant_unit = "ms"
        band_layout = list(record.payload["band_layout"])
        origin_time = {
            "kind": "ms",
            "bits": time_quant_bits,
            "sample_rate": record.sample_rate,
            "hop_length": record.hop_length,
        }
        segment_hashes = list(record.payload["segment_hashes"])

    payload: dict[str, Any] = {
        "track_id": record.track_id,
        "segment_index": record.segment_index,
        "frame_number": record.frame_number,
        "sample_rate": record.sample_rate,
        "hop_length": record.hop_length,
        "peak_pairs": [pair.as_dict() for pair in record.peak_pairs],
        "hash_algo": record.hash_algo,
        "segment_hash": record.segment_hash,
        "segment_hashes": segment_hashes,
        "time_quant_bits": time_quant_bits,
        "band_layout": band_layout,
        "time_quant_unit": time_quant_unit,
        "channel_merge": context.channel_merge,
        "compression": dict(context.compression),
        "origin_version": record.version,
        "origin_checksum": record.checksum,
        "origin_time_quantization": origin_time,
        "migration_context": {
            "channel_merge": "explicit-context",
            "compression": "explicit-context",
        },
        "migration": {
            "tool": "fpstore.migrate",
            "source_version": record.version,
            "target_version": 3,
            "time_quantization_preserved": True,
        },
    }
    target_id = f"{record.track_id}:v3:seg{record.segment_index:06d}"
    raw = _seal_record(3, target_id, payload)
    target = parse_record(raw)

    outcome = MigrationOutcome(
        source_record_id=record.record_id,
        source_version=record.version,
        target_record=target,
        field_mapping=mapping,
        lost_fields=lost,
    )
    return outcome


def migrate_records(
    records: Iterable[FingerprintRecord],
    context: MigrationContext,
    *,
    strict: bool = True,
) -> MigrationReport:
    outcomes = [migrate_record(record, context) for record in records]
    report = MigrationReport(outcomes)
    if strict and report.lost_fields:
        raise BizError(
            E_MIGRATE_LOSS,
            "migration would lose fields without an explicit target mapping",
            {
                "affected_records": report.affected_records,
                "lost_fields": report.lost_fields,
                "report": report.as_dict(),
            },
        )
    return report
