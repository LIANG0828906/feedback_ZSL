"""Block-wise deterministic stereo master rendering with snapshot resume."""

from __future__ import annotations

import hashlib
import json
import os

import numpy as np
from scipy.signal import sosfilt, sosfilt_zi

from .bands import bandpass_sos
from .errors import BusinessError, E_RESUME_MISMATCH
from .wavio import open_rw, write_wav

FADE_MS = 5.0


def stable_hash_bytes(payload):
    return hashlib.sha256(payload).hexdigest()


def stable_hash_json(obj):
    return stable_hash_bytes(
        json.dumps(obj, sort_keys=True, separators=(",", ":")).encode("utf-8")
    )


def mic_pan_gain(position):
    """Equal-power stereo pan derived from the mic x-coordinate angle."""
    angle = np.arctan2(position[0], max(1e-9, np.hypot(position[1], position[2])))
    angle = np.clip(angle, -np.pi / 2.0, np.pi / 2.0)
    left = float(np.cos(angle))
    right = float(np.sin(angle))
    norm = np.hypot(left, right)
    return left / norm, right / norm


def build_event_weights(events, ratio_table, band_defs, mic_names, sample_rate, total_samples):
    """Per-sample band/mic weight lookup, with short equal-power fades."""
    ordered = sorted(mic_names)
    fade = int(round(FADE_MS / 1000.0 * sample_rate))
    weight_shape = (total_samples, len(band_defs), len(ordered))
    weights = np.zeros(weight_shape, dtype=np.float64)
    activity = np.zeros(total_samples, dtype=np.float64)
    ratio_by_event = {entry["event_index"]: entry for entry in ratio_table}
    for event in events:
        start = int(round(event["start"] * sample_rate))
        end = int(round(event["end"] * sample_rate))
        entry = ratio_by_event[event["event_index"]]
        ramp = np.ones(end - start, dtype=np.float64)
        ramp_len = min(fade, (end - start) // 2)
        if ramp_len > 0:
            ramp[:ramp_len] = np.linspace(0.0, 1.0, ramp_len)
            ramp[-ramp_len:] = np.linspace(1.0, 0.0, ramp_len)
        activity[start:end] = np.maximum(activity[start:end], ramp)
        for band_index, band_entry in enumerate(entry["bands"]):
            for mic_index, name in enumerate(ordered):
                weights[start:end, band_index, mic_index] = (
                    ramp * band_entry["ratios"][name]
                )
    return weights, activity, ordered


class BandStream:
    """Streaming per-mic band-pass filter whose state rebuilds from snapshot."""

    def __init__(self, signal, sos):
        self.signal = signal
        self.sos = sos
        self.zi = sosfilt_zi(sos) * signal[0]
        self.cursor = 0

    def fast_forward(self, sample_count):
        """Reproduce filter state as if sample_count frames had been processed."""
        if sample_count > 0:
            _, self.zi = sosfilt(self.sos, self.signal[:sample_count], zi=self.zi)
            self.cursor = sample_count

    def process(self, block):
        start = self.cursor
        end = start + len(block)
        out, self.zi = sosfilt(self.sos, self.signal[start:end], zi=self.zi)
        self.cursor = end
        return out


def input_digest(project, mic_files):
    """Content digest of the project declaration plus every WAV byte stream."""
    with open(project["project_path"], "rb") as handle:
        digest = hashlib.sha256(handle.read())
    for path in mic_files:
        with open(path, "rb") as handle:
            digest.update(handle.read())
    return digest.hexdigest()


def render_master(project, events, ratio_table, mic_signals, output_path,
                  snapshot_path, interrupted=None):
    """Render the pre-limiter stereo master in fixed blocks with snapshots.

    `interrupted` is an optional callable returning True once, used to emulate
    Ctrl+C: the current block still completes before the snapshot is committed.
    Returns (path, snapshot_dict, interrupted_flag).
    """
    sr = project["sample_rate"]
    total_samples = max(len(sig) for sig in mic_signals.values())
    block_samples = max(1, int(round(project["block_seconds"] * sr)))
    band_defs = project["freq_bands"]
    mic_names = [mic["name"] for mic in project["microphones"]]
    weights, activity, ordered = build_event_weights(
        events, ratio_table, band_defs, mic_names, sr, total_samples
    )
    noise_gain = 10.0 ** (project["noise_gain_db"] / 20.0)
    pans = {name: mic_pan_gain(next(m["position"] for m in project["microphones"]
                                    if m["name"] == name)) for name in ordered}

    sos_list = [bandpass_sos(band["low_hz"], band["high_hz"], sr) for band in band_defs]
    streams = {
        name: [BandStream(mic_signals[name], sos) for sos in sos_list]
        for name in ordered
    }

    digest = input_digest(project, [mic["file"] for mic in project["microphones"]])
    snapshot_payload = {
        "input_digest": digest,
        "completed_blocks": 0,
        "total_samples": total_samples,
        "events": events,
        "ratio_table": ratio_table,
        "band_defs": band_defs,
        "noise_gain_db": project["noise_gain_db"],
        "block_samples": block_samples,
    }

    resume_count = 0
    start_block = 0
    if os.path.isfile(snapshot_path):
        with open(snapshot_path, "r", encoding="utf-8") as handle:
            old = json.load(handle)
        if (old.get("input_digest") != digest
                or old.get("events") != events
                or old.get("ratio_table") != ratio_table
                or old.get("total_samples") != total_samples):
            # Whole pipeline must restart; stale block offsets are never reused.
            for stale in (snapshot_path, output_path):
                if os.path.isfile(stale):
                    os.remove(stale)
            raise BusinessError(
                E_RESUME_MISMATCH,
                "snapshot does not match current project or detected events",
                {"snapshot": os.path.basename(snapshot_path)},
            )
        start_block = int(old["completed_blocks"])
        resume_count = int(old.get("resume_count", 0)) + 1
        for name in ordered:
            fast_forward_to = min(start_block * block_samples, total_samples)
            for stream in streams[name]:
                stream.fast_forward(fast_forward_to)

    if not os.path.isfile(output_path):
        write_wav(output_path, np.zeros((total_samples, 2), dtype=np.float64), sr)

    total_blocks = int(np.ceil(total_samples / block_samples))
    got_interrupt = False
    completed = start_block
    out_handle = open_rw(output_path)
    try:
        out_handle.seek(completed * block_samples)
        for block_index in range(start_block, total_blocks):
            start = block_index * block_samples
            end = min(total_samples, start + block_samples)
            length = end - start
            mix = np.zeros((length, 2), dtype=np.float64)
            for mic_index, name in enumerate(ordered):
                band_blocks = [stream.process(np.arange(length)) for stream in streams[name]]
                raw = mic_signals[name][start:end]
                left_pan, right_pan = pans[name]
                for band_index, band_block in enumerate(band_blocks):
                    band_weight = weights[start:end, band_index, mic_index]
                    mono = band_block * band_weight
                    mix[:, 0] += mono * left_pan
                    mix[:, 1] += mono * right_pan
                # Outside detected events the declared ambience bed is retained;
                # the 5 ms activity ramp avoids any on/off step.
                ambience = raw * noise_gain * (1.0 - activity[start:end])
                mix[:, 0] += ambience * left_pan
                mix[:, 1] += ambience * right_pan
            out_handle.write(mix)
            completed = block_index + 1
            snapshot_payload["completed_blocks"] = completed
            snapshot_payload["resume_count"] = resume_count
            commit_snapshot(snapshot_path, snapshot_payload)
            if interrupted is not None and interrupted():
                got_interrupt = True
                break
    finally:
        out_handle.close()
    return output_path, snapshot_payload, got_interrupt, resume_count


def commit_snapshot(snapshot_path, payload):
    """Atomic snapshot commit: temp file then os.replace."""
    tmp = snapshot_path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, sort_keys=True, separators=(",", ":"))
    os.replace(tmp, snapshot_path)
