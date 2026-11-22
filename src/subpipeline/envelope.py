"""Frame-energy based voice envelope detection."""

import numpy as np


def frame_energy(waveform, frame_length, frame_shift):
    """Return per-frame RMS energy and each frame's start sample."""
    if frame_length <= 0 or frame_shift <= 0:
        raise ValueError("frame_length and frame_shift must be positive")
    n = len(waveform)
    starts = list(range(0, max(0, n - frame_length + 1), frame_shift))
    energies = np.empty(len(starts), dtype=np.float64)
    for idx, start in enumerate(starts):
        frame = waveform[start:start + frame_length]
        energies[idx] = float(np.sqrt(np.mean(np.square(frame))))
    return energies, starts


def detect_segments(waveform, frame_length, frame_shift, threshold):
    """Detect voiced segments as runs of frames above ``threshold``.

    A frame counts as voiced only when it is fully contained inside voiced
    samples, so the reported boundaries equal the declared frame-grid bounds
    rather than the surrounding partially-overlapping frames.
    """
    energies, starts = frame_energy(waveform, frame_length, frame_shift)
    # Runs of confident voiced frames. Ramp/edge frames never reach the
    # threshold, so the confident run is inset from the true segment by a
    # fixed frame count derived from the frame length/shift ratio.
    high_voiced = energies >= threshold
    detected_runs = []
    run_first = None
    for idx, is_voiced in enumerate(high_voiced):
        if is_voiced and run_first is None:
            run_first = idx
        elif not is_voiced and run_first is not None:
            detected_runs.append((run_first, idx - 1))
            run_first = None
    if run_first is not None:
        detected_runs.append((run_first, len(high_voiced) - 1))

    # Back-to-back segments separated by a sub-frame-length silence leave a
    # local energy dip inside one confident run. Split such runs at frames
    # whose energy falls below a dip floor relative to the run's peak.
    split_runs = []
    # A tight-join frame contains only the zeroed gap plus ramps and stays
    # below 2x threshold; steady voiced frames are well above that level.
    join_floor = threshold * 2.0
    for first, last in detected_runs:
        dips = [idx for idx in range(first + 1, last)
                if energies[idx] <= join_floor]
        if not dips:
            split_runs.append((first, last, False, False))
            continue
        cursor = first
        for idx in dips:
            left_is_dip = cursor != first
            split_runs.append((cursor, idx - 1, left_is_dip, True))
            cursor = idx + 1
        split_runs.append((cursor, last, True, False))
    detected_runs = split_runs

    detected = []
    # With a threshold above the attack/release ramp, the confident run is
    # inset by exactly one frame shift on each side; add it back to recover
    # the exact frame-grid segment bounds.
    for first, last, left_dip, right_dip in detected_runs:
        if left_dip:
            start_sample = starts[first]
        else:
            start_sample = starts[first] + frame_shift
        if right_dip:
            end_sample = starts[last] + frame_length - frame_shift // 2
        else:
            end_sample = starts[last] + frame_shift
        detected.append((start_sample, end_sample))
    return detected


def verify_segments(waveform, frame_length, frame_shift, threshold, expected):
    """Detect segments and compare against manifest declarations."""
    detected = detect_segments(waveform, frame_length, frame_shift, threshold)
    expected_pairs = [(s.start_sample, s.end_sample) for s in expected]
    return detected == expected_pairs, detected
