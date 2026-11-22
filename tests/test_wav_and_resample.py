from __future__ import annotations

import hashlib
import struct
import unittest
from pathlib import Path

import numpy as np

from live_transcode.errors import E_WAV_INVALID, TaskError
from live_transcode.models import ResampleRatio, TierParams
from live_transcode.resample import render_output_range, render_slice
from live_transcode.sidecar import sidecar_bytes
from live_transcode.wavio import inspect_wav, load_wav

from .support import Workspace, find_chunks


class WavValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.ctx = Workspace("lt-wav-")
        cls.ctx.__enter__()
        cls.fixtures = cls.ctx.fixtures(seed=41, duration=1.0)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.ctx.__exit__(None, None, None)

    def _mutated(self, fixture_index: int, mutation) -> Path:
        source = Path(self.fixtures[fixture_index]["path"])
        raw = bytearray(source.read_bytes())
        chunks = find_chunks(raw)
        fmt_offset, _ = chunks["fmt "]
        data_offset, _ = chunks["data"]
        mutation(raw, fmt_offset, data_offset)
        target = self.ctx.root / f"invalid-{fixture_index}-{len(list(self.ctx.root.glob('invalid-*')))}.wav"
        target.write_bytes(raw)
        return target

    def test_generated_layouts_and_depths(self) -> None:
        expected = {
            (1, 16, 0x4),
            (1, 24, 0x4),
            (6, 16, 0x3F),
            (6, 24, 0x3F),
        }
        observed = set()
        for fixture in self.fixtures:
            info, samples = load_wav(fixture["path"])
            observed.add((info.channels, info.bits_per_sample, info.channel_mask))
            self.assertEqual(info.sample_rate, 48000)
            self.assertEqual(samples.shape, (info.frames, info.channels))
            self.assertLessEqual(float(np.max(np.abs(samples))), 1.0)
        self.assertEqual(observed, expected)

    def test_mono_channel_mask_mismatch(self) -> None:
        path = self._mutated(
            0,
            lambda raw, fmt, data: struct.pack_into("<I", raw, fmt + 20, 0x3F),
        )
        with self.assertRaises(TaskError) as raised:
            inspect_wav(path)
        self.assertEqual(raised.exception.code, E_WAV_INVALID)
        self.assertEqual(raised.exception.field, "fmt.dwChannelMask")

    def test_surround_channel_mask_mismatch(self) -> None:
        path = self._mutated(
            2,
            lambda raw, fmt, data: struct.pack_into("<I", raw, fmt + 20, 0x4),
        )
        with self.assertRaises(TaskError) as raised:
            inspect_wav(path)
        self.assertEqual(raised.exception.code, E_WAV_INVALID)
        self.assertEqual(raised.exception.field, "fmt.dwChannelMask")

    def test_bits_per_sample_mismatch(self) -> None:
        path = self._mutated(
            0,
            lambda raw, fmt, data: struct.pack_into("<H", raw, fmt + 14, 20),
        )
        with self.assertRaises(TaskError) as raised:
            inspect_wav(path)
        self.assertEqual(raised.exception.code, E_WAV_INVALID)
        self.assertEqual(raised.exception.field, "fmt.bitsPerSample")

    def test_block_alignment_mismatch(self) -> None:
        path = self._mutated(
            0,
            lambda raw, fmt, data: struct.pack_into("<H", raw, fmt + 12, 3),
        )
        with self.assertRaises(TaskError) as raised:
            inspect_wav(path)
        self.assertEqual(raised.exception.field, "fmt.blockAlign")

    def test_data_length_not_whole_frames(self) -> None:
        path = self._mutated(
            2,
            lambda raw, fmt, data: struct.pack_into(
                "<I", raw, data - 4, struct.unpack_from("<I", raw, data - 4)[0] - 1
            ),
        )
        with self.assertRaises(TaskError) as raised:
            inspect_wav(path)
        self.assertEqual(raised.exception.field, "data.size")


class ResampleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.ctx = Workspace("lt-resample-")
        cls.ctx.__enter__()
        cls.fixtures = cls.ctx.fixtures(seed=77, duration=1.4)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.ctx.__exit__(None, None, None)

    def test_slice_history_matches_global_range(self) -> None:
        info, source = load_wav(self.fixtures[1]["path"])
        tier = TierParams("test", 32000, 40, 24, 1, 257)
        total = ResampleRatio.build(48000, 32000).output_length(info.frames)
        global_render = render_output_range(source, info, tier, 0, total, total)
        assembled = bytearray()
        previous = None
        index = 0
        while index * tier.slice_samples < total:
            rendered = render_slice(source, info, tier, index, total)
            if previous is not None:
                self.assertLess(
                    rendered.src_start,
                    previous.src_end,
                    "filter support must overlap the previous slice",
                )
            assembled.extend(rendered.pcm_bytes)
            previous = rendered
            index += 1
        self.assertEqual(bytes(assembled), global_render.pcm_bytes)
        self.assertEqual(
            hashlib.sha256(bytes(assembled)).hexdigest(),
            hashlib.sha256(global_render.pcm_bytes).hexdigest(),
        )

    def test_phase_remainder_is_derived_from_previous_end(self) -> None:
        info, source = load_wav(self.fixtures[0]["path"])
        tier = TierParams("test", 44100, 48, 16, 1, 256)
        total = ResampleRatio.build(48000, 44100).output_length(info.frames)
        ratio = ResampleRatio.build(48000, 44100)
        previous_end = None
        for index in range((total + tier.slice_samples - 1) // tier.slice_samples):
            rendered = render_slice(source, info, tier, index, total)
            if previous_end is None:
                expected = (
                    rendered.phase_num + (tier.filter_length // 2) * ratio.step_num
                ) % ratio.step_den
            else:
                expected = (
                    previous_end + (tier.filter_length // 2) * ratio.step_num
                ) % ratio.step_den
                self.assertEqual(rendered.phase_num, previous_end)
            self.assertEqual(rendered.phase_remainder_num, expected)
            previous_end = rendered.end_phase_num

    def test_content_hash_is_hash_of_pcm_bytes(self) -> None:
        info, source = load_wav(self.fixtures[3]["path"])
        rendered = render_slice(
            source,
            info,
            TierParams("test", 24000, 32, 24, 1, 512),
            3,
        )
        self.assertEqual(
            rendered.content_hash,
            hashlib.sha256(rendered.pcm_bytes).hexdigest(),
        )
        self.assertTrue(sidecar_bytes({"x": 1}).endswith(b"\n"))


if __name__ == "__main__":
    unittest.main()

