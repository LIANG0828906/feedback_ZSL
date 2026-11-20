"""Strict parsers and writers for v1/v2/v3 fingerprint records."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .config import CHANNEL_MERGE_STRATEGIES, FingerprintConfig
from .errors import BizError, E_FP_INVALID
from .fingerprint import (
    ExtractedFingerprint,
    HashHit,
    PeakPair,
    SegmentFingerprint,
    make_pair_hash,
    segment_content_hash,
)
from .util import canonical_bytes, content_hash, hash_label


RECORD_SCHEMA = "fp-record/1"
HASH_ALGOS = ("v1-pair-raw", "v2-pairs-quant")

V1_FIELDS = {
    "track_id",
    "segment_index",
    "frame_number",
    "sample_rate",
    "hop_length",
    "peak_pairs",
}
V2_EXTRA_FIELDS = {
    "hash_algo",
    "segment_hash",
    "segment_hashes",
    "time_quant_bits",
    "band_layout",
}
V3_EXTRA_FIELDS = {
    "time_quant_unit",
    "channel_merge",
    "compression",
    "origin_version",
    "origin_checksum",
    "origin_time_quantization",
    "migration",
    "migration_context",
}


@dataclass(frozen=True)
class RecordTimeBasis:
    kind: str
    sample_rate: int
    hop_length: int
    bits: int | None = None

    def as_dict(self) -> dict[str, Any]:
        if self.kind == "frame":
            return {"kind": "frame", "sample_rate": self.sample_rate, "hop_length": self.hop_length}
        return {"kind": "ms", "sample_rate": self.sample_rate, "hop_length": self.hop_length, "bits": self.bits}


@dataclass(frozen=True)
class FingerprintRecord:
    version: int
    record_id: str
    checksum: str
    payload: dict[str, Any]
    peak_pairs: tuple[PeakPair, ...]
    hashes: tuple[HashHit, ...]
    segment_hash: str
    time_basis: RecordTimeBasis
    hash_algo: str
    track_id: str
    segment_index: int
    sample_rate: int
    hop_length: int
    frame_number: int
    raw: dict[str, Any]

    @property
    def key(self) -> str:
        return record_key(self.track_id, self.version, self.hop_length, self.sample_rate, self.segment_index)

    def to_dict(self) -> dict[str, Any]:
        return dict(self.raw)

    def to_bytes(self) -> bytes:
        return canonical_bytes(self.raw)

    def check_extra_fields(self) -> dict[str, Any]:
        known = set(V1_FIELDS)
        if self.version >= 2:
            known |= V2_EXTRA_FIELDS
        if self.version >= 3:
            known |= V3_EXTRA_FIELDS
        return {key: value for key, value in self.payload.items() if key not in known}


def record_key(track_id: str, version: int, hop_length: int, sample_rate: int, segment_index: int) -> str:
    return f"{track_id}|v{version}|h{hop_length}|sr{sample_rate}|s{segment_index:06d}"


def _record_payload(
    segment: SegmentFingerprint,
    fingerprint: ExtractedFingerprint,
    config: FingerprintConfig,
    *,
    compression: dict[str, Any],
    origin: dict[str, Any] | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "track_id": segment.track_id,
        "segment_index": segment.segment_index,
        "frame_number": segment.frame_number,
        "sample_rate": fingerprint.sample_rate,
        "hop_length": config.hop_length,
        "peak_pairs": [pair.as_dict() for pair in segment.peak_pairs],
    }
    if config.version >= 2:
        payload.update(
            {
                "hash_algo": fingerprint.hash_algo,
                "segment_hash": segment.segment_hash,
                "segment_hashes": [hit.as_dict() for hit in segment.hashes],
                "time_quant_bits": config.time_quant_bits,
                "band_layout": list(config.band_edges_hz),
            }
        )
    if config.version >= 3:
        payload.update(
            {
                "time_quant_unit": "frame" if config.time_quant_bits is None else "ms",
                "channel_merge": "mono" if fingerprint.channels == 1 else fingerprint.channel_merge,
                "compression": dict(compression),
            }
        )
        if origin:
            payload.update(origin)
    return payload


def _seal_record(version: int, record_id: str, payload: dict[str, Any]) -> dict[str, Any]:
    envelope = {
        "schema": RECORD_SCHEMA,
        "version": version,
        "record_id": record_id,
        "payload": payload,
    }
    return {**envelope, "checksum": hash_label(content_hash(envelope))}


def pcm_compression_metadata(bit_depth: int = 16) -> dict[str, Any]:
    return {"codec": "pcm_s16le" if bit_depth == 16 else f"pcm_s{bit_depth}le", "bit_depth": bit_depth, "lossless": True}


def build_record(
    segment: SegmentFingerprint,
    fingerprint: ExtractedFingerprint,
    config: FingerprintConfig,
    *,
    compression: dict[str, Any] | None = None,
    origin: dict[str, Any] | None = None,
) -> FingerprintRecord:
    if compression is None:
        compression = pcm_compression_metadata(16)
    record_id = f"{segment.track_id}:v{config.version}:seg{segment.segment_index:06d}"
    payload = _record_payload(segment, fingerprint, config, compression=compression, origin=origin)
    raw = _seal_record(config.version, record_id, payload)
    return parse_record(raw)


def _fail(record_id: Any, field: str, message: str, **extra: Any) -> None:
    details = {"record_id": str(record_id), "field": field, "reason": message}
    details.update(extra)
    raise BizError(E_FP_INVALID, message, details)


def _require(mapping: dict[str, Any], field: str, record_id: Any, expected: type | tuple[type, ...] = object) -> Any:
    if field not in mapping:
        _fail(record_id, field, "required field is missing")
    value = mapping[field]
    if expected is not object and (not isinstance(value, expected) or isinstance(value, bool) and expected is not bool):
        _fail(record_id, field, "field has invalid type", expected=str(expected), actual=type(value).__name__)
    return value


def _int_field(
    mapping: dict[str, Any],
    field: str,
    record_id: Any,
    minimum: int | None = None,
    *,
    path: str | None = None,
) -> int:
    try:
        value = _require(mapping, field, record_id, int)
    except BizError as exc:
        if path is not None:
            exc.details["field"] = path
        raise
    if minimum is not None and value < minimum:
        _fail(record_id, path or field, "integer is below minimum", minimum=minimum, value=value)
    return value


def _parse_pairs(payload: dict[str, Any], record_id: Any) -> tuple[PeakPair, ...]:
    raw_pairs = _require(payload, "peak_pairs", record_id, list)
    pairs: list[PeakPair] = []
    for index, raw in enumerate(raw_pairs):
        location = f"peak_pairs[{index}]"
        if not isinstance(raw, dict):
            _fail(record_id, location, "peak pair must be an object")
        pair = PeakPair(
            anchor_frame=_int_field(raw, "anchor_frame", record_id, 0, path=f"{location}.anchor_frame"),
            t1_frame=_int_field(raw, "t1_frame", record_id, 0, path=f"{location}.t1_frame"),
            t2_frame=_int_field(raw, "t2_frame", record_id, 0, path=f"{location}.t2_frame"),
            f1_bin=_int_field(raw, "f1_bin", record_id, 0, path=f"{location}.f1_bin"),
            f2_bin=_int_field(raw, "f2_bin", record_id, 0, path=f"{location}.f2_bin"),
        )
        if pair.anchor_frame != pair.t1_frame:
            _fail(record_id, f"{location}.anchor_frame", "anchor_frame must equal t1_frame")
        if pair.t2_frame <= pair.t1_frame:
            _fail(record_id, f"{location}.t2_frame", "t2_frame must be greater than t1_frame")
        pairs.append(pair)
    return tuple(pairs)


def _derive_v1_hashes(
    pairs: Iterable[PeakPair],
    sample_rate: int,
    hop_length: int,
) -> tuple[HashHit, ...]:
    hashes: list[HashHit] = []
    for pair in pairs:
        delta_frames = pair.t2_frame - pair.t1_frame
        digest = make_pair_hash(
            hash_algo="v1-pair-raw",
            f1_bin=pair.f1_bin,
            f2_bin=pair.f2_bin,
            delta_units=delta_frames,
            band1=-1,
            band2=-1,
            time_quant_bits=None,
            band_edges=None,
        )
        offset_ms = (pair.anchor_frame * hop_length * 1000) // sample_rate
        hashes.append(
            HashHit(
                hash=digest,
                offset_ms=int(offset_ms),
                offset_ticks=pair.anchor_frame,
                delta_units=delta_frames,
                t2_frame=pair.t2_frame,
                f1_bin=pair.f1_bin,
                f2_bin=pair.f2_bin,
                band1=-1,
                band2=-1,
                anchor_frame=pair.anchor_frame,
            )
        )
    return tuple(sorted(hashes, key=lambda hit: (hit.offset_ticks, hit.hash, hit.f1_bin, hit.f2_bin)))


def _parse_hashes(
    payload: dict[str, Any],
    record_id: Any,
    time_basis: RecordTimeBasis,
) -> tuple[HashHit, ...]:
    raw_hashes = _require(payload, "segment_hashes", record_id, list)
    hashes: list[HashHit] = []
    for index, raw in enumerate(raw_hashes):
        location = f"segment_hashes[{index}]"
        if not isinstance(raw, dict):
            _fail(record_id, location, "segment hash must be an object")
        digest = _require(raw, "hash", record_id, str)
        if len(digest) < 8 or any(char not in "0123456789abcdef" for char in digest):
            _fail(record_id, f"{location}.hash", "hash must be lowercase hexadecimal")
        offset_ms = _int_field(raw, "offset_ms", record_id, 0)
        offset_ticks = _int_field(raw, "offset_ticks", record_id, 0)
        if time_basis.kind == "ms":
            quantum = 1 << int(time_basis.bits or 0)
            if offset_ms != offset_ticks * quantum:
                _fail(
                    record_id,
                    f"{location}.offset_ticks",
                    "offset_ticks and offset_ms disagree with time quantization",
                    offset_ms=offset_ms,
                    offset_ticks=offset_ticks,
                    quantum_ms=quantum,
                )
        hashes.append(
            HashHit(
                hash=digest,
                offset_ms=offset_ms,
                offset_ticks=offset_ticks,
                delta_units=_int_field(raw, "delta_units", record_id, 0),
                t2_frame=_int_field(raw, "t2_frame", record_id, 0),
                f1_bin=_int_field(raw, "f1_bin", record_id, 0),
                f2_bin=_int_field(raw, "f2_bin", record_id, 0),
                band1=_int_field(raw, "band1", record_id, -1),
                band2=_int_field(raw, "band2", record_id, -1),
                anchor_frame=_int_field(raw, "anchor_frame", record_id, 0),
            )
        )
    return tuple(sorted(hashes, key=lambda hit: (hit.offset_ticks, hit.hash, hit.f1_bin, hit.f2_bin)))


def _validate_band_layout(value: Any, record_id: Any) -> tuple[int, ...]:
    if not isinstance(value, list):
        _fail(record_id, "band_layout", "band_layout must be a list")
    edges = []
    for index, edge in enumerate(value):
        if not isinstance(edge, int) or isinstance(edge, bool) or edge < 0:
            _fail(record_id, f"band_layout[{index}]", "band edge must be a nonnegative integer")
        edges.append(edge)
    if edges:
        if edges[0] != 0:
            _fail(record_id, "band_layout[0]", "band layout must start at 0 Hz")
        if any(left >= right for left, right in zip(edges, edges[1:])):
            _fail(record_id, "band_layout", "band edges must be strictly increasing")
    return tuple(edges)


def _validate_compression(value: Any, record_id: Any) -> None:
    if not isinstance(value, dict):
        _fail(record_id, "compression", "compression metadata must be an object")
    codec = _require(value, "codec", record_id, str)
    if not codec:
        _fail(record_id, "compression.codec", "compression codec must not be empty")
    _int_field(value, "bit_depth", record_id, 0)
    _require(value, "lossless", record_id, bool)


def parse_record(raw: dict[str, Any]) -> FingerprintRecord:
    if not isinstance(raw, dict):
        _fail("<unknown>", "<record>", "fingerprint record must be an object")
    record_id = raw.get("record_id", "<unknown>")
    if "schema" not in raw:
        _fail(record_id, "schema", "required field is missing")
    if raw["schema"] != RECORD_SCHEMA:
        _fail(record_id, "schema", "unsupported record schema", value=raw["schema"])
    if "version" not in raw:
        _fail(record_id, "version", "required field is missing")
    version = raw["version"]
    if isinstance(version, bool) or not isinstance(version, int) or version not in (1, 2, 3):
        _fail(record_id, "version", "unknown fingerprint version", value=version)
    record_id = _require(raw, "record_id", record_id, str)
    if "checksum" not in raw:
        _fail(record_id, "checksum", "required field is missing")
    checksum = raw["checksum"]
    if not isinstance(checksum, str):
        _fail(record_id, "checksum", "checksum must be a string")
    payload = _require(raw, "payload", record_id, dict)

    try:
        core = {
            "schema": raw["schema"],
            "version": raw["version"],
            "record_id": raw["record_id"],
            "payload": payload,
        }
        expected_checksum = hash_label(content_hash(core))
    except (TypeError, ValueError) as exc:
        _fail(record_id, "payload", "record is not canonically hashable", reason=str(exc))
    if checksum != expected_checksum:
        _fail(
            record_id,
            "checksum",
            "record checksum does not match canonical content",
            expected=expected_checksum,
            actual=checksum,
        )

    track_id = _require(payload, "track_id", record_id, str)
    if not track_id:
        _fail(record_id, "track_id", "track_id must not be empty")
    segment_index = _int_field(payload, "segment_index", record_id, 0)
    frame_number = _int_field(payload, "frame_number", record_id, 0)
    sample_rate = _int_field(payload, "sample_rate", record_id, 8000)
    if sample_rate > 48000:
        _fail(record_id, "sample_rate", "sample_rate exceeds 48000", value=sample_rate)
    hop_length = _int_field(payload, "hop_length", record_id, 1)
    pairs = _parse_pairs(payload, record_id)

    if version == 1:
        hash_algo = "v1-pair-raw"
        time_basis = RecordTimeBasis("frame", sample_rate, hop_length, None)
        hashes = _derive_v1_hashes(pairs, sample_rate, hop_length)
        segment_hash = segment_content_hash(pairs, hashes)
    else:
        hash_algo = _require(payload, "hash_algo", record_id, str)
        if hash_algo not in HASH_ALGOS:
            _fail(record_id, "hash_algo", "unknown hash algorithm", value=hash_algo)
        if "time_quant_bits" not in payload:
            _fail(record_id, "time_quant_bits", "required field is missing")
        bits = payload["time_quant_bits"]
        if bits is not None and (isinstance(bits, bool) or not isinstance(bits, int) or not 0 <= bits <= 16):
            _fail(record_id, "time_quant_bits", "time_quant_bits must be in 0..16", value=bits)
        _validate_band_layout(_require(payload, "band_layout", record_id, list), record_id)
        if version == 2:
            if bits is None:
                _fail(record_id, "time_quant_bits", "v2 requires a millisecond quantization width")
            time_basis = RecordTimeBasis("ms", sample_rate, hop_length, bits)
        else:
            unit = _require(payload, "time_quant_unit", record_id, str)
            if unit == "ms":
                time_basis = RecordTimeBasis("ms", sample_rate, hop_length, bits)
            elif unit == "frame":
                if bits is not None:
                    _fail(record_id, "time_quant_bits", "frame time basis requires null time_quant_bits")
                time_basis = RecordTimeBasis("frame", sample_rate, hop_length, None)
            else:
                _fail(record_id, "time_quant_unit", "unknown time quantization unit", value=unit)
        hashes = _parse_hashes(payload, record_id, time_basis)
        segment_hash = _require(payload, "segment_hash", record_id, str)
        actual_segment_hash = segment_content_hash(pairs, hashes)
        if segment_hash != actual_segment_hash:
            _fail(
                record_id,
                "segment_hash",
                "segment content hash mismatch",
                expected=actual_segment_hash,
                actual=segment_hash,
            )
        if version == 3:
            channel_merge = _require(payload, "channel_merge", record_id, str)
            if channel_merge not in CHANNEL_MERGE_STRATEGIES:
                _fail(record_id, "channel_merge", "unknown channel merge marker", value=channel_merge)
            _validate_compression(_require(payload, "compression", record_id, dict), record_id)

    return FingerprintRecord(
        version=version,
        record_id=record_id,
        checksum=checksum,
        payload=payload,
        peak_pairs=pairs,
        hashes=hashes,
        segment_hash=segment_hash,
        time_basis=time_basis,
        hash_algo=hash_algo,
        track_id=track_id,
        segment_index=segment_index,
        sample_rate=sample_rate,
        hop_length=hop_length,
        frame_number=frame_number,
        raw=raw,
    )


def parse_record_bytes(data: bytes) -> FingerprintRecord:
    import json

    try:
        raw = json.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise BizError(
            E_FP_INVALID,
            "fingerprint record is not valid UTF-8 JSON",
            {"record_id": "<unknown>", "field": "<record>", "reason": str(exc)},
        ) from exc
    return parse_record(raw)


def guard_unknown_versions(records: Iterable[dict[str, Any]]) -> None:
    for index, record in enumerate(records):
        if not isinstance(record, dict) or "version" not in record:
            record_id = record.get("record_id", f"<record#{index}>") if isinstance(record, dict) else f"<record#{index}>"
            _fail(record_id, "version", "required field is missing")
        if record["version"] not in (1, 2, 3):
            _fail(
                record.get("record_id", f"<record#{index}>"),
                "version",
                "unknown fingerprint version",
                value=record["version"],
            )
