"""v3 fingerprint format: channel-merge flag + compression metadata,
unified time quantization baseline, optional legacy preservation block.

Layout:
{
  "format_version": 3,
  "payload": {
    ... core fields ...,
    "peak_pairs": [...], "hashes": [{"hash": int, "t_ms": number}, ...],
    "time_quant_ms": number, "bands_hz": [...],
    "channel_merge": bool,
    "compression": {"codec": str, "level": int, "original_bytes": int} | null,
    "provenance": {"extractor": str, "params": {...}, "source": str},
    "legacy": {"source_version": int, "time_quant_ms": number|null,
               "quant_bits": int|null, "extras": {...}} | null
  },
  "checksum": sha256(payload)
}
"""

from __future__ import annotations

from ..errors import E_FMT_INVALID, FpError
from ..model import Fingerprint, PeakPair
from .common import require, require_type, verify_checksum, wrap_payload

VERSION = 3


def _parse_compression(value, file):
    if value is None:
        return None
    if not isinstance(value, dict):
        raise FpError(E_FMT_INVALID, "compression must be an object or null",
                      file=file, field="payload.compression")
    for key in ("codec", "level", "original_bytes"):
        if key not in value:
            raise FpError(E_FMT_INVALID, f"missing compression key '{key}'",
                          file=file, field=f"payload.compression.{key}")
    if not isinstance(value["codec"], str):
        raise FpError(E_FMT_INVALID, "compression.codec must be a string",
                      file=file, field="payload.compression.codec")
    for key in ("level", "original_bytes"):
        if not isinstance(value[key], int) or isinstance(value[key], bool):
            raise FpError(E_FMT_INVALID, f"compression.{key} must be an int",
                          file=file, field=f"payload.compression.{key}")
    return dict(value)


def parse(doc: dict, file: str) -> Fingerprint:
    version = require(doc, "format_version", "format_version", file)
    if version != VERSION:
        raise FpError(E_FMT_INVALID,
                      f"v3 parser got format_version={version!r}",
                      file=file, field="format_version")
    payload = verify_checksum(doc, file)

    sample_id = require_type(payload, "sample_id", (str,), "payload.sample_id", file)
    sample_rate = require_type(payload, "sample_rate", (int,), "payload.sample_rate", file)
    channels = require_type(payload, "channels", (int,), "payload.channels", file)
    duration_ms = require_type(payload, "duration_ms", (int, float), "payload.duration_ms", file)
    frame_length = require_type(payload, "frame_length", (int,), "payload.frame_length", file)
    hop_length = require_type(payload, "hop_length", (int,), "payload.hop_length", file)
    time_quant_ms = require_type(payload, "time_quant_ms", (int, float),
                                 "payload.time_quant_ms", file)
    bands_hz = require_type(payload, "bands_hz", (list,), "payload.bands_hz", file)
    channel_merge = require_type(payload, "channel_merge", (bool,),
                                 "payload.channel_merge", file)
    compression = _parse_compression(require(payload, "compression",
                                             "payload.compression", file), file)
    provenance = require_type(payload, "provenance", (dict,),
                              "payload.provenance", file)
    raw_pairs = require_type(payload, "peak_pairs", (list,), "payload.peak_pairs", file)
    raw_hashes = require_type(payload, "hashes", (list,), "payload.hashes", file)

    peak_pairs = []
    for i, row in enumerate(raw_pairs):
        field = f"payload.peak_pairs[{i}]"
        if not (isinstance(row, list) and len(row) == 4
                and all(isinstance(v, int) and not isinstance(v, bool) for v in row)):
            raise FpError(E_FMT_INVALID,
                          "peak pair must be [anchor_frame, freq1, freq2, delta_frames] of ints",
                          file=file, field=field)
        peak_pairs.append(PeakPair(*row))

    hashes = []
    for i, entry in enumerate(raw_hashes):
        field = f"payload.hashes[{i}]"
        if not isinstance(entry, dict) or "hash" not in entry or "t_ms" not in entry:
            raise FpError(E_FMT_INVALID,
                          "hash entry must be an object with 'hash' and 't_ms'",
                          file=file, field=field)
        h, t = entry["hash"], entry["t_ms"]
        if not isinstance(h, int) or isinstance(h, bool):
            raise FpError(E_FMT_INVALID, "'hash' must be an int",
                          file=file, field=f"{field}.hash")
        if not isinstance(t, (int, float)) or isinstance(t, bool):
            raise FpError(E_FMT_INVALID, "'t_ms' must be a number",
                          file=file, field=f"{field}.t_ms")
        hashes.append((h, float(t)))
    if not hashes:
        raise FpError(E_FMT_INVALID, "fingerprint contains no hashes",
                      file=file, field="payload.hashes")

    legacy = payload.get("legacy")
    fp = Fingerprint(
        sample_id=sample_id, sample_rate=sample_rate, channels=channels,
        duration_ms=float(duration_ms), frame_length=frame_length,
        hop_length=hop_length, time_quant_ms=float(time_quant_ms),
        peak_pairs=peak_pairs, hashes=hashes, bands_hz=tuple(bands_hz),
        channel_merge=channel_merge, compression=compression,
        provenance=dict(provenance), legacy=legacy or {}, source_version=3,
    )
    return fp


def serialize(fp: Fingerprint) -> dict:
    payload = {
        "sample_id": fp.sample_id,
        "sample_rate": fp.sample_rate,
        "channels": fp.channels,
        "duration_ms": fp.duration_ms,
        "frame_length": fp.frame_length,
        "hop_length": fp.hop_length,
        "time_quant_ms": fp.time_quant_ms,
        "bands_hz": list(fp.bands_hz),
        "channel_merge": fp.channel_merge,
        "compression": fp.compression,
        "provenance": fp.provenance or {"extractor": None, "params": {},
                                        "source": "unknown"},
        "peak_pairs": [[p.anchor_frame, p.freq1, p.freq2, p.delta_frames]
                       for p in fp.peak_pairs],
        "hashes": [{"hash": h, "t_ms": t} for h, t in fp.hashes],
        "legacy": fp.legacy or None,
    }
    doc = {"format_version": VERSION}
    doc.update(wrap_payload(payload))
    return doc
