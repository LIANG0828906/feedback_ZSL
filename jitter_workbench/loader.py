"""Sample loading and arrival-pattern validation.

Error mapping (see errors.py):
  E_META   - missing field, sample-rate mismatch, non-monotonic declared
             media timestamps, declared per-frame sample count mismatch,
             negative arrival time
  E_JITTER - unknown jitter pattern tag (whole sample rejected)
  E_DECODE - frame payload stream corrupted / wrong byte size
  E_ORDER  - media timestamp inversion in the arrival stream that is not
             declared via a ``reorder`` tag
  E_DUP    - duplicate arrivals beyond the configured tolerance
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass

import numpy as np

from .errors import (DecodeError, DupError, JitterError, MetaError,
                     OrderError)
from .sample_gen import KNOWN_JITTER_PATTERNS, sample_content_hash

REQUIRED_FIELDS = ("sample_id", "sample_rate", "frame_len", "hop",
                   "num_frames", "duration_samples", "frames", "arrivals",
                   "frames_file")


@dataclass
class Arrival:
    seq: int
    media_ts: int
    arrival_ts: int
    dup: bool


class Sample:
    """Validated, immutable view of one deterministic sample."""

    def __init__(self, sample_dir, manifest, frames, truth_wave,
                 truth_energy, arrivals):
        self.dir = sample_dir
        self.manifest = manifest
        self.frames = frames                # (num_frames, frame_len) f32
        self.truth_wave = truth_wave
        self.truth_energy = truth_energy
        self.arrivals = arrivals            # sorted by (arrival_ts, seq)
        self.sample_rate = manifest["sample_rate"]
        self.frame_len = manifest["frame_len"]
        self.hop = manifest["hop"]
        self.num_frames = manifest["num_frames"]
        self.duration_samples = manifest["duration_samples"]
        self.hop_ms = self.hop * 1000 // self.sample_rate
        self.content_hash = sample_content_hash(sample_dir)

    def frame_data(self, seq):
        return self.frames[seq]


def _require(cond, field, msg):
    if not cond:
        raise MetaError(msg, field=field)


def load_sample(sample_dir, *, dup_tolerance_ratio=0.10,
                dup_tolerance_abs=20):
    manifest_path = os.path.join(sample_dir, "manifest.json")
    try:
        with open(manifest_path, "r") as fh:
            manifest = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise DecodeError("manifest unreadable: %s" % exc,
                          field="manifest.json")

    # ---- required fields ------------------------------------------------
    for name in REQUIRED_FIELDS:
        _require(name in manifest, name, "missing required field: %s" % name)

    rate = manifest["sample_rate"]
    frame_len = manifest["frame_len"]
    hop = manifest["hop"]
    num_frames = manifest["num_frames"]

    _require(isinstance(rate, int) and rate > 0, "sample_rate",
             "sample_rate must be a positive integer")
    _require((hop * 1000) % rate == 0, "hop",
             "hop must map to an integer number of milliseconds")

    # cross-check sample rate against truth metadata
    truth_meta_path = os.path.join(sample_dir, "truth.json")
    if os.path.exists(truth_meta_path):
        with open(truth_meta_path) as fh:
            truth_meta = json.load(fh)
        _require(truth_meta.get("sample_rate") == rate, "sample_rate",
                 "sample_rate mismatch between manifest and truth.json")

    # ---- declared frame table --------------------------------------------
    frames_decl = manifest["frames"]
    _require(len(frames_decl) == num_frames, "frames",
             "frames table length != num_frames")
    prev_ts = -1
    for entry in frames_decl:
        for key in ("seq", "media_ts", "n_samples"):
            _require(key in entry, "frames.%s" % key,
                     "frame entry missing field: %s" % key)
        _require(entry["media_ts"] > prev_ts, "frames.media_ts",
                 "declared media timestamps must be strictly increasing")
        prev_ts = entry["media_ts"]
        _require(entry["n_samples"] == frame_len, "frames.n_samples",
                 "frame %d declares %d samples, frame_len is %d"
                 % (entry["seq"], entry["n_samples"], frame_len))

    # ---- jitter tags -------------------------------------------------------
    declared_patterns = set(manifest.get("jitter_patterns", []))
    unknown = declared_patterns - set(KNOWN_JITTER_PATTERNS)
    if unknown:
        raise JitterError(
            "unknown jitter pattern(s): %s - sample rejected"
            % ",".join(sorted(unknown)),
            field="jitter_patterns",
            detail={"unknown": sorted(unknown)})
    for entry in frames_decl:
        bad = set(entry.get("tags", [])) - set(KNOWN_JITTER_PATTERNS)
        if bad:
            raise JitterError(
                "unknown jitter tag(s) on frame %d: %s - sample rejected"
                % (entry["seq"], ",".join(sorted(bad))),
                field="frames.tags",
                detail={"unknown": sorted(bad), "seq": entry["seq"]})

    # ---- arrival stream ----------------------------------------------------
    arrivals_raw = manifest["arrivals"]
    arrivals = []
    dup_count = 0
    prev_media = -1
    tag_by_seq = {e["seq"]: set(e.get("tags", [])) for e in frames_decl}
    for rec in arrivals_raw:
        for key in ("seq", "media_ts", "arrival_ts"):
            _require(key in rec, "arrivals.%s" % key,
                     "arrival record missing field: %s" % key)
        _require(rec["arrival_ts"] >= 0, "arrivals.arrival_ts",
                 "arrival_ts must be non-negative (seq=%d)" % rec["seq"])
        _require(0 <= rec["seq"] < num_frames, "arrivals.seq",
                 "arrival seq out of range: %d" % rec["seq"])
        _require(rec["media_ts"] == frames_decl[rec["seq"]]["media_ts"],
                 "arrivals.media_ts",
                 "arrival media_ts does not match declaration (seq=%d)"
                 % rec["seq"])
        dup = bool(rec.get("dup", False))
        dup_count += 1 if dup else 0
        # media-time inversion must be declared as reorder
        if rec["media_ts"] < prev_media:
            declared = "reorder" in tag_by_seq.get(rec["seq"], set())
            if not declared:
                raise OrderError(
                    "media timestamp inversion at seq=%d (media_ts=%d < %d) "
                    "without reorder declaration"
                    % (rec["seq"], rec["media_ts"], prev_media),
                    field="arrivals.media_ts",
                    detail={"seq": rec["seq"]})
        prev_media = max(prev_media, rec["media_ts"])
        arrivals.append(Arrival(seq=rec["seq"], media_ts=rec["media_ts"],
                                arrival_ts=rec["arrival_ts"], dup=dup))

    tol = max(dup_tolerance_abs, int(dup_tolerance_ratio * num_frames))
    if dup_count > tol:
        raise DupError(
            "duplicate arrivals %d exceed tolerance %d" % (dup_count, tol),
            field="arrivals",
            detail={"duplicates": dup_count, "tolerance": tol})

    # arrivals must be sorted by (arrival_ts, seq, dup) - the receive view
    arrivals.sort(key=lambda a: (a.arrival_ts, a.seq, a.dup))

    # ---- frame payload ------------------------------------------------------
    frames_path = os.path.join(sample_dir, manifest["frames_file"])
    expected_bytes = num_frames * frame_len * 4
    try:
        size = os.path.getsize(frames_path)
    except OSError as exc:
        raise DecodeError("frames file unreadable: %s" % exc,
                          field="frames_file")
    if size != expected_bytes:
        raise DecodeError(
            "frames file size %d != expected %d (stream corrupted)"
            % (size, expected_bytes), field="frames_file",
            detail={"size": size, "expected": expected_bytes})
    try:
        frames = np.fromfile(frames_path, dtype="<f4").reshape(
            num_frames, frame_len)
    except (OSError, ValueError) as exc:
        raise DecodeError("frames decode failed: %s" % exc,
                          field="frames_file")

    truth_wave = np.load(os.path.join(sample_dir, manifest["truth_wave"]))
    truth_energy = np.load(os.path.join(sample_dir,
                                        manifest["truth_energy"]))
    _require(truth_wave.shape[0] == manifest["duration_samples"],
             "duration_samples", "truth waveform length mismatch")
    _require(truth_energy.shape[0] == num_frames, "frames",
             "truth energy length mismatch")

    return Sample(sample_dir, manifest, frames, truth_wave, truth_energy,
                  arrivals)
