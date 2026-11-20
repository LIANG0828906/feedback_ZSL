"""Deterministic synthetic sample generator.

Generates multi-sample-rate, mono/stereo synthetic WAVs plus v1/v2/v3
baseline fingerprint files. A fixed seed makes every artifact reproducible.
"""

from __future__ import annotations

import json
import os

import numpy as np

from .extract import extract_fingerprint
from .formats import save_fingerprint
from .wavio import write_wav


def _tone_track(rng, duration_s, sample_rate):
    """A deterministic mixture of harmonics + chirps, distinctive per seed."""
    t = np.arange(int(duration_s * sample_rate)) / sample_rate
    sig = np.zeros_like(t)
    n_parts = 3 + int(rng.randint(0, 3))
    for _ in range(n_parts):
        f0 = rng.uniform(300, 3000)
        amp = rng.uniform(0.15, 0.35)
        kind = rng.randint(0, 2)
        if kind == 0:
            sig += amp * np.sin(2 * np.pi * f0 * t)
            sig += amp * 0.4 * np.sin(2 * np.pi * 2 * f0 * t)
        else:
            f1 = f0 + rng.uniform(-800, 800)
            k = (f1 - f0) / duration_s
            sig += amp * np.sin(2 * np.pi * (f0 * t + 0.5 * k * t * t))
    sig += 0.01 * rng.standard_normal(len(t))
    peak = np.max(np.abs(sig))
    return (sig / max(peak, 1e-9) * 0.9).astype(np.float32)


def generate_corpus(root, seed=42):
    """Generate the deterministic demo corpus. Returns a manifest dict."""
    rng = np.random.RandomState(seed)
    wav_dir = os.path.join(root, "wav")
    fp_dir = os.path.join(root, "baselines")
    os.makedirs(wav_dir, exist_ok=True)
    os.makedirs(fp_dir, exist_ok=True)

    track_specs = [
        ("track_alpha", 8000, 1, 6.0),
        ("track_beta", 16000, 2, 6.0),
        ("track_gamma", 44100, 2, 6.0),
    ]
    tracks = {}
    manifest = {"seed": seed, "samples": [], "queries": []}
    for name, sr, ch, dur in track_specs:
        sig = _tone_track(rng, dur, sr)
        path = os.path.join(wav_dir, f"{name}.wav")
        write_wav(path, sig, sr, channels=ch)
        tracks[name] = (sig, sr)
        manifest["samples"].append(
            {"sample_id": name, "wav": os.path.relpath(path, root),
             "sample_rate": sr, "channels": ch})

    query_specs = [
        ("query_alpha", "track_alpha", 2.0, 2.5),
        ("query_beta", "track_beta", 1.5, 2.5),
        ("query_gamma", "track_gamma", 3.0, 2.0),
    ]
    for qname, src, offset_s, dur_s in query_specs:
        sig, sr = tracks[src]
        seg = sig[int(offset_s * sr): int((offset_s + dur_s) * sr)].copy()
        seg += 0.005 * rng.standard_normal(len(seg)).astype(np.float32)
        path = os.path.join(wav_dir, f"{qname}.wav")
        write_wav(path, seg, sr, channels=1)
        manifest["queries"].append({
            "query_id": qname, "source_sample": src,
            "truth_offset_ms": round(offset_s * 1000, 3),
            "wav": os.path.relpath(path, root),
        })

    version_of = {"track_alpha": 1, "track_beta": 2, "track_gamma": 3}
    for name, version in version_of.items():
        fp = extract_fingerprint(os.path.join(wav_dir, f"{name}.wav"), name)
        path = os.path.join(fp_dir, f"{name}.v{version}.json")
        save_fingerprint(fp, path, version=version)
        for s in manifest["samples"]:
            if s["sample_id"] == name:
                s["baseline"] = os.path.relpath(path, root)
                s["version"] = version

    with open(os.path.join(root, "corpus_manifest.json"), "w",
              encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)
        fh.write("\n")
    return manifest
