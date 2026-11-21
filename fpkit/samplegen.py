"""Deterministic synthetic WAV sample generator (seeded, no external data)."""

import json
import wave
from pathlib import Path

import numpy as np

RATES = [8000, 16000, 22050, 44100, 48000]


def _tone(freq, dur, rate, amp=0.5):
    t = np.arange(int(round(dur * rate))) / rate
    return amp * np.sin(2 * np.pi * freq * t) + 0.2 * amp * np.sin(4 * np.pi * freq * t)


def make_snippet(rng):
    """A known melody: 8 tones whose frequencies come from the seeded rng."""
    freqs = rng.uniform(300.0, 3000.0, size=8).round(1)
    return [(float(f), 0.12) for f in freqs]


def render_snippet(snippet, rate):
    parts = [_tone(f, d, rate, amp=0.6) for f, d in snippet]
    return np.concatenate(parts)


def _background(rng, rate, dur):
    n = int(round(dur * rate))
    sig = 0.02 * rng.standard_normal(n)
    for f in rng.uniform(150.0, 2500.0, size=3):
        sig = sig + _tone(float(f), dur, rate, amp=0.08)[:n]
    return sig


def _to_int16(sig):
    return (np.clip(sig, -1.0, 1.0) * 32767.0).astype("<i2")


def write_wav(path, sig, rate, channels, sampwidth=2):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    if channels == 2:
        rng = np.random.default_rng(0)  # deterministic stereo decorrelation
        right = 0.8 * sig + 0.02 * rng.standard_normal(len(sig))
        frames = np.column_stack([_to_int16(sig), _to_int16(right)]).reshape(-1)
        data = frames.astype("<i2").tobytes()
    elif sampwidth == 1:
        data = ((np.clip(sig, -1, 1) * 127) + 128).astype(np.uint8).tobytes()
    else:
        data = _to_int16(sig).tobytes()
    with wave.open(str(path), "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(sampwidth)
        wf.setframerate(rate)
        wf.writeframes(data)


def generate(workdir, seed, n_samples=12, n_queries=4):
    """Generate samples/queries/invalid files + ground truth. Deterministic."""
    workdir = Path(workdir)
    sample_dir = workdir / "samples"
    rng = np.random.default_rng(seed)
    snippet = make_snippet(rng)
    snippet_dur = sum(d for _, d in snippet)
    manifest = {"seed": seed, "snippet": snippet, "samples": [], "queries": [],
                "invalid": []}
    for i in range(n_samples):
        rate = RATES[i % len(RATES)]
        channels = 1 + (i % 2)
        dur = 2.0 + 0.5 * (i % 3)
        sig = _background(rng, rate, dur)
        entry = {"sample_id": f"sample-{i:03d}", "rate": rate,
                 "channels": channels, "duration": dur,
                 "path": f"samples/sample-{i:03d}.wav"}
        if i % 3 == 0:  # embed the known snippet at a known offset
            offset = round(0.3 + 0.2 * (i % 4), 2)
            snip = render_snippet(snippet, rate)
            start = int(offset * rate)
            sig[start:start + len(snip)] += snip
            entry["snippet_offset_sec"] = offset
        write_wav(sample_dir / f"sample-{i:03d}.wav", sig, rate, channels)
        manifest["samples"].append(entry)
    snippet_rates = [e["rate"] for e in manifest["samples"]
                     if "snippet_offset_sec" in e]
    for j in range(n_queries):
        rate = snippet_rates[j % len(snippet_rates)]
        sig = render_snippet(snippet, rate) + 0.02 * rng.standard_normal(
            int(round(snippet_dur * rate)))
        qid = f"query-{j:02d}"
        write_wav(sample_dir / f"{qid}.wav", sig, rate, 1)
        manifest["queries"].append({"query_id": qid, "rate": rate,
                                    "path": f"samples/{qid}.wav",
                                    "expect_offset_sec": 0.0})
    # unsupported layouts for E_AUDIO_UNSUPPORTED checks
    sig = _background(rng, 8000, 0.5)
    write_wav(sample_dir / "bad-rate.wav", sig, 96000, 1)
    manifest["invalid"].append({"path": "samples/bad-rate.wav",
                                "field": "sample_rate"})
    write_wav(sample_dir / "bad-width.wav", sig, 16000, 1, sampwidth=1)
    manifest["invalid"].append({"path": "samples/bad-width.wav",
                                "field": "bits_per_sample"})
    write_wav(sample_dir / "bad-channels.wav", sig, 16000, 3)
    manifest["invalid"].append({"path": "samples/bad-channels.wav",
                                "field": "channels"})
    with open(workdir / "ground_truth.json", "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return manifest
