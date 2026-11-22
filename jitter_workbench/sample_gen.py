"""Deterministic voice-sample generator.

Produces, for a fixed seed:
  * manifest.json   - declared frame stream + arrival pattern + jitter tags
  * frames.bin      - float32 LE frame payloads (num_frames * frame_len)
  * truth_wave.npy   - per-sample original waveform (OLA ground truth)
  * truth_energy.npy- per-frame energy ground truth
  * truth.json      - truth-side metadata (sample_rate cross-check)

The arrival list is the receive-side view: drops removed, duplicates
added, reorder applied, per-frame arrival delays attached.
"""
from __future__ import annotations

import hashlib
import json
import os

import numpy as np

KNOWN_JITTER_PATTERNS = ("delay", "reorder", "drop", "duplicate", "surge")

SAMPLE_RATE = 16000
FRAME_LEN = 320          # 20 ms
HOP = 160                # 10 ms
NUM_FRAMES = 300
SEED = 42


def _synth_waveform(rng, num_frames, hop, frame_len, rate):
    """Three-segment synthetic voice-like waveform (float32)."""
    n = (num_frames - 1) * hop + frame_len
    t = np.arange(n, dtype=np.float64) / rate
    third = n // 3
    f0 = np.empty(n, dtype=np.float64)
    f0[:third] = 140.0                       # calm segment
    f0[third:2 * third] = 220.0
    f0[2 * third:] = 180.0
    phase = 2 * np.pi * np.cumsum(f0) / rate
    wave = (0.55 * np.sin(phase)
            + 0.28 * np.sin(2 * phase + 0.3)
            + 0.14 * np.sin(3 * phase + 1.1))
    # syllable-ish envelope
    env = 0.6 + 0.4 * np.sin(2 * np.pi * 3.0 * t) ** 2
    noise = rng.standard_normal(n) * 0.02
    return (wave * env + noise).astype(np.float32)


def generate_sample(out_dir: str, seed: int = SEED,
                    num_frames: int = NUM_FRAMES) -> dict:
    """Generate one deterministic sample set; returns the manifest."""
    os.makedirs(out_dir, exist_ok=True)
    rng = np.random.default_rng(seed)
    rate, frame_len, hop = SAMPLE_RATE, FRAME_LEN, HOP
    hop_ms = hop * 1000 // rate

    wave = _synth_waveform(rng, num_frames, hop, frame_len, rate)
    frames = np.stack([wave[k * hop: k * hop + frame_len]
                       for k in range(num_frames)]).astype(np.float32)
    energy = np.sum(frames.astype(np.float64) ** 2, axis=1)

    # ---- deterministic jitter arrival pattern -------------------------
    seg = num_frames // 3
    arrival = np.empty(num_frames, dtype=np.int64)
    tags: list[list[str]] = [[] for _ in range(num_frames)]
    for k in range(num_frames):
        media_ms = k * hop_ms
        if k < seg:                                # calm: 20-40 ms
            d = int(rng.integers(20, 41))
        elif k < 2 * seg:                          # surge: 20-140 ms spikes
            d = int(rng.integers(20, 141))
            tags[k].append("surge")
        else:                                      # tail: 30-60 ms
            d = int(rng.integers(30, 61))
        arrival[k] = media_ms + d
        tags[k].append("delay")

    # reorder: swap arrival times of deterministic adjacent pairs in surge
    reorder_pairs = [(seg + 10, seg + 11), (seg + 25, seg + 26),
                     (seg + 40, seg + 41)]
    for a, b in reorder_pairs:
        arrival[a], arrival[b] = arrival[b], arrival[a]
        tags[a].append("reorder")
        tags[b].append("reorder")

    # drops: deterministic frames never arrive
    drop_frames = sorted(int(x) for x in
                         rng.choice(np.arange(2 * seg, num_frames),
                                    size=8, replace=False))
    drop_frames += [seg + 5, seg + 55]
    drop_frames = sorted(set(drop_frames))
    for k in drop_frames:
        tags[k].append("drop")

    # duplicates: deterministic frames arrive twice
    dup_frames = sorted(int(x) for x in
                        rng.choice(np.arange(0, seg), size=5, replace=False))
    for k in dup_frames:
        tags[k].append("duplicate")

    # ---- manifest ------------------------------------------------------
    frame_entries = [{
        "seq": k,
        "media_ts": k * hop_ms,
        "n_samples": frame_len,
        "tags": sorted(set(tags[k])),
    } for k in range(num_frames)]

    arrivals = []
    for k in range(num_frames):
        if k in drop_frames:
            continue
        arrivals.append({"seq": k, "media_ts": k * hop_ms,
                         "arrival_ts": int(arrival[k]), "dup": False})
    for k in dup_frames:
        arrivals.append({"seq": k, "media_ts": k * hop_ms,
                         "arrival_ts": int(arrival[k]) + 3, "dup": True})
    arrivals.sort(key=lambda r: (r["arrival_ts"], r["seq"], r["dup"]))

    # truthfully tag every frame whose arrival order inverts media order
    max_seen = -1
    for rec in arrivals:
        if rec["media_ts"] < max_seen:
            tags[rec["seq"]].append("reorder")
        max_seen = max(max_seen, rec["media_ts"])

    # refresh declared frame tags now that reorder tagging is complete
    for entry, tg in zip(frame_entries, tags):
        entry["tags"] = sorted(set(tg))

    manifest = {
        "sample_id": "voice_seed%d" % seed,
        "seed": seed,
        "sample_rate": rate,
        "frame_len": frame_len,
        "hop": hop,
        "num_frames": num_frames,
        "duration_samples": int((num_frames - 1) * hop + frame_len),
        "jitter_patterns": sorted(set(
            t for tg in tags for t in tg)),
        "frames_file": "frames.bin",
        "truth_wave": "truth_wave.npy",
        "truth_energy": "truth_energy.npy",
        "frames": frame_entries,
        "arrivals": arrivals,
    }

    frames.tofile(os.path.join(out_dir, "frames.bin"))
    np.save(os.path.join(out_dir, "truth_wave.npy"), wave)
    np.save(os.path.join(out_dir, "truth_energy.npy"),
            energy.astype(np.float64))
    with open(os.path.join(out_dir, "truth.json"), "w") as fh:
        json.dump({"sample_rate": rate, "frame_len": frame_len, "hop": hop,
                   "num_frames": num_frames}, fh, indent=1)
    with open(os.path.join(out_dir, "manifest.json"), "w") as fh:
        json.dump(manifest, fh, indent=1)
    return manifest


def sample_content_hash(sample_dir: str) -> str:
    """sha256 over manifest (canonical) + frame payload bytes."""
    h = hashlib.sha256()
    with open(os.path.join(sample_dir, "manifest.json"), "rb") as fh:
        h.update(fh.read())
    with open(os.path.join(sample_dir, "frames.bin"), "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


if __name__ == "__main__":
    import sys
    out = sys.argv[1] if len(sys.argv) > 1 else "samples/voice_seed42"
    m = generate_sample(out)
    print("generated", m["sample_id"], "frames:", m["num_frames"],
          "arrivals:", len(m["arrivals"]))
