"""v1 fingerprint format: raw spectral peak pairs with frame numbers.

Layout:
{
  "format_version": 1,
  "payload": {
    "sample_id": str, "sample_rate": int, "channels": int,
    "duration_ms": number, "frame_length": int, "hop_length": int,
    "peak_pairs": [[anchor_frame, freq1, freq2, delta_frames], ...]
  },
  "checksum": sha256(payload)
}
"""

from __future__ import annotations

from ..errors import E_FMT_INVALID, FpError
from ..model import Fingerprint, PeakPair
from .common import require, require_type, verify_checksum, wrap_payload

VERSION = 1


def parse(doc: dict, file: str) -> Fingerprint:
    version = require(doc, "format_version", "format_version", file)
    if version != VERSION:
        raise FpError(E_FMT_INVALID,
                      f"v1 parser got format_version={version!r}",
                      file=file, field="format_version")
    payload = verify_checksum(doc, file)

    sample_id = require_type(payload, "sample_id", (str,), "payload.sample_id", file)
    sample_rate = require_type(payload, "sample_rate", (int,), "payload.sample_rate", file)
    channels = require_type(payload, "channels", (int,), "payload.channels", file)
    duration_ms = require_type(payload, "duration_ms", (int, float), "payload.duration_ms", file)
    frame_length = require_type(payload, "frame_length", (int,), "payload.frame_length", file)
    hop_length = require_type(payload, "hop_length", (int,), "payload.hop_length", file)
    raw_pairs = require_type(payload, "peak_pairs", (list,), "payload.peak_pairs", file)

    peak_pairs = []
    for i, row in enumerate(raw_pairs):
        field = f"payload.peak_pairs[{i}]"
        if not (isinstance(row, list) and len(row) == 4
                and all(isinstance(v, int) and not isinstance(v, bool) for v in row)):
            raise FpError(E_FMT_INVALID,
                          "peak pair must be [anchor_frame, freq1, freq2, delta_frames] of ints",
                          file=file, field=field)
        peak_pairs.append(PeakPair(*row))
    if not peak_pairs:
        raise FpError(E_FMT_INVALID, "fingerprint contains no peak pairs",
                      file=file, field="payload.peak_pairs")

    fp = Fingerprint(
        sample_id=sample_id, sample_rate=sample_rate, channels=channels,
        duration_ms=float(duration_ms), frame_length=frame_length,
        hop_length=hop_length, peak_pairs=peak_pairs, source_version=1,
    )
    fp.hashes = [(p.hash(), fp.quantize_time(p.anchor_frame)) for p in peak_pairs]
    fp.provenance = {"source": "v1-file", "extractor": None}
    return fp


def serialize(fp: Fingerprint) -> dict:
    payload = {
        "sample_id": fp.sample_id,
        "sample_rate": fp.sample_rate,
        "channels": fp.channels,
        "duration_ms": fp.duration_ms,
        "frame_length": fp.frame_length,
        "hop_length": fp.hop_length,
        "peak_pairs": [[p.anchor_frame, p.freq1, p.freq2, p.delta_frames]
                       for p in fp.peak_pairs],
    }
    doc = {"format_version": VERSION}
    doc.update(wrap_payload(payload))
    return doc
