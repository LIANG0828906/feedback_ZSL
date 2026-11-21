"""要求 3：切片产物生成与逐片内容哈希确定性。"""

from __future__ import annotations

from pathlib import Path

import numpy as np

from mediaaudit.parameters import TierParams
from mediaaudit.samplegen import _pcm_samples, SampleSpec
from mediaaudit.slicing import (
    apply_gain,
    generate_all,
    generate_slice,
    linear_resample,
    quantized_slice_bounds,
    serialize_artifact,
)
from mediaaudit.timebase import build_intervals, SourceTimebase


def _source(rate: int = 16000, channels: int = 1, duration: float = 1.0) -> SourceTimebase:
    spec = SampleSpec("t", rate, channels, duration, 0.25)
    audio = _pcm_samples(spec)
    count = audio.shape[0]
    ticks = [int(i * 0.25 * 1_000_000) for i in range(int(duration / 0.25))]
    intervals = build_intervals(rate, count, ticks)
    return SourceTimebase(
        wav_path=Path("x.wav"),
        frames_path=Path("x.json"),
        sample_rate=rate,
        channels=channels,
        sample_count=count,
        samples=audio,
        frame_times_ticks=ticks,
        intervals=intervals,
    )


def test_repeat_generation_byte_identical() -> None:
    source = _source()
    params = TierParams(16000, 256, 1000, 250_000)
    first = generate_all(source, "tier", 1, params)
    second = generate_all(source, "tier", 1, params)
    assert len(first) == len(second)
    for a, b in zip(first, second):
        assert a.content_hash_value == b.content_hash_value
        assert serialize_artifact(a) == serialize_artifact(b)
        assert np.array_equal(a.audio, b.audio)


def test_hash_independent_of_wallclock_and_param_version_field() -> None:
    source = _source()
    params = TierParams(16000, 256, 1000, 250_000)
    a = generate_slice(source, "tier", 1, params, 0, 0, 250_000)
    b = generate_slice(source, "tier", 99, params, 0, 0, 250_000)
    # 版本号不进入内容哈希，相同内容必须同哈希。
    assert a.content_hash_value == b.content_hash_value


def test_gain_changes_hash_but_bounds_stay() -> None:
    source = _source()
    p1 = TierParams(16000, 256, 1000, 250_000)
    p2 = TierParams(16000, 400, 1000, 250_000)
    a = generate_slice(source, "tier", 1, p1, 0, 0, 250_000)
    b = generate_slice(source, "tier", 2, p2, 0, 0, 250_000)
    assert a.content_hash_value != b.content_hash_value
    assert a.source_start_sample == b.source_start_sample
    assert a.source_end_sample == b.source_end_sample


def test_resample_target_count_matches_rate_ratio() -> None:
    source = _source(48000)
    out = linear_resample(
        source.samples[:24000], 48000, 24000, 0, 500_000
    )
    assert out.shape[0] == 12000
    assert out.dtype == np.int16


def test_gain_saturation_is_deterministic() -> None:
    audio = np.array([[32000], [-32000]], dtype="<i2")
    gained = apply_gain(audio, 512)
    assert gained.max() <= 32767 and gained.min() >= -32768
    assert np.array_equal(gained, apply_gain(audio, 512))


def test_quantized_bounds_cover_full_duration() -> None:
    source = _source(16000, duration=1.0)
    params = TierParams(16000, 256, 1000, 300_000)
    bounds = quantized_slice_bounds(source, params)
    assert bounds[0][0] == 0
    assert bounds[-1][1] == source.duration_ticks
