"""Section-aware stereo mixing with bounded crossfades."""

from __future__ import annotations

import numpy as np

from . import (
    GROUP_OF_PART,
    GROUP_PERCUSSION,
    GROUP_VOCAL,
    GROUP_YUEQIN,
    PART_BED,
)

# Constant-power pan positions (-1 left .. +1 right).
PAN = {
    GROUP_PERCUSSION: 0.0,
    GROUP_VOCAL: -0.35,
    GROUP_YUEQIN: 0.45,
}


def _pan_gains(position):
    angle = (position + 1.0) * np.pi / 4.0
    return float(np.cos(angle)), float(np.sin(angle))


def _section_gain_envelope(cue, n):
    sr = cue.sample_rate
    groups = (GROUP_PERCUSSION, GROUP_VOCAL, GROUP_YUEQIN)
    env = {g: np.zeros(n) for g in groups}
    boundaries = [0]
    for sec in cue.sections:
        start = min(int(round(sec.start * sr)), n)
        end = min(int(round(sec.end * sr)), n)
        boundaries.append((start, end, sec))
    sections = cue.sections
    if not sections:
        for g in groups:
            env[g][:] = 1.0
        return env

    # Fill each section with its group gains.
    for i, sec in enumerate(sections):
        start = min(int(round(sec.start * sr)), n)
        end = min(int(round(sec.end * sr)), n)
        for g in groups:
            gain_db = sec.group_gains_db.get(g, 0.0)
            env[g][start:end] = 10.0 ** (gain_db / 20.0)

    # Crossfades only at interior section boundaries, capped by declared time.
    max_fade = int(round(cue.crossfade_seconds * sr))
    for i in range(1, len(sections)):
        boundary = min(int(round(sections[i].start * sr)), n)
        fade = min(max_fade, boundary, n - boundary)
        if fade <= 1:
            continue
        ramp = np.linspace(0.0, 1.0, fade)
        for g in groups:
            prev_gain = 10.0 ** (
                sections[i - 1].group_gains_db.get(g, 0.0) / 20.0
            )
            next_gain = 10.0 ** (sections[i].group_gains_db.get(g, 0.0) / 20.0)
            env[g][boundary - fade : boundary] = (
                prev_gain * (1.0 - ramp) + next_gain * ramp
            )

    # Short edge fades to guarantee no boundary clicks (<= crossfade time).
    edge = min(max_fade, n // 8) if n else 0
    if edge > 1:
        ramp_in = np.linspace(0.0, 1.0, edge)
        ramp_out = np.linspace(1.0, 0.0, edge)
        for g in groups:
            env[g][:edge] *= ramp_in
            env[g][-edge:] *= ramp_out
    return env


def mix_master(signals, cue):
    """Render the stereo master at the longest-channel sample count."""
    sr = cue.sample_rate
    n = max(x.shape[0] for x in signals.values())
    env = _section_gain_envelope(cue, n)
    master = np.zeros((n, 2), dtype=np.float64)
    group_sums = {
        GROUP_PERCUSSION: np.zeros(n),
        GROUP_VOCAL: np.zeros(n),
        GROUP_YUEQIN: np.zeros(n),
    }
    for part, x in signals.items():
        if part == PART_BED:
            continue
        padded = np.zeros(n)
        padded[: x.shape[0]] = x
        group_sums[GROUP_OF_PART[part]] += padded

    # The bed contributes ambience to both channels at a fixed low level.
    bed = np.zeros(n)
    bed[: signals[PART_BED].shape[0]] = signals[PART_BED]
    master[:, 0] += bed * 0.5
    master[:, 1] += bed * 0.5

    for group, mono in group_sums.items():
        left_g, right_g = _pan_gains(PAN[group])
        gain = env[group]
        master[:, 0] += mono * gain * left_g
        master[:, 1] += mono * gain * right_g
    return master
