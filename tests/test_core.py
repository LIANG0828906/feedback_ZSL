from __future__ import annotations

import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

from audio_repair.audio_io import AudioData, load_audio
from audio_repair.config import RepairConfig, SessionConfig, default_config
from audio_repair.dsp import denoise_segment, reconstruct_without_gain
from audio_repair.engine import (
    RepairSession,
    RunStatus,
    process_full_recompute,
    process_realtime_with_checkpoint,
)
from audio_repair.errors import AppError
from audio_repair.metrics import audio_stats, locate_first_difference
from audio_repair.samples import generate_deterministic_samples
from audio_repair.segments import SegmentStateMachine, SegmentStatus, split_segments


class CoreTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.sample_dir = cls.root / "samples"
        cls.manifest = generate_deterministic_samples(cls.sample_dir)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_fixtures_regenerate_byte_identically(self) -> None:
        regenerated = self.root / "regenerated"
        manifest = generate_deterministic_samples(regenerated)
        self.assertEqual(
            [item["sha256"] for item in self.manifest["samples"]],
            [item["sha256"] for item in manifest["samples"]],
        )

    def test_stft_exact_timeline_for_short_and_tail_inputs(self) -> None:
        rng = np.random.default_rng(11)
        configs = [
            RepairConfig(frame_size=256, hop_size=64, window="hann", strength=0.0),
            RepairConfig(frame_size=512, hop_size=128, window="hamming", strength=0.0),
            RepairConfig(frame_size=512, hop_size=128, window="blackman", strength=0.0),
            RepairConfig(
                frame_size=512, hop_size=512, window="rectangular", strength=0.0
            ),
        ]
        for sample_count in (1, 7, 1000, 8193, 16385):
            for channels in (1, 2):
                samples = rng.normal(0.0, 0.1, (sample_count, channels))
                for config in configs:
                    with self.subTest(
                        sample_count=sample_count,
                        channels=channels,
                        window=config.window,
                    ):
                        output = reconstruct_without_gain(samples, config)
                        self.assertEqual(output.shape, samples.shape)
                        self.assertLessEqual(
                            float(np.max(np.abs(output - samples))), 1e-10
                        )

    def test_processed_segment_has_exact_length(self) -> None:
        audio = load_audio(self.sample_dir / "boundary_stereo_tail1.wav")
        output = denoise_segment(audio.samples, default_config("music")).output
        self.assertEqual(output.shape, audio.samples.shape)

    def test_strength_is_monotonic(self) -> None:
        audio = load_audio(self.sample_dir / "speech_noisy_mono.wav")
        base = default_config("speech")
        rms_values = []
        for strength in (0.0, 0.5, 1.0, 2.0, 3.0, 4.0):
            config = RepairConfig(
                **{**base.canonical_dict(), "strength": float(strength)}
            )
            result = denoise_segment(audio.samples, config)
            rms_values.append(float(np.sqrt(np.mean(result.output * result.output))))
        self.assertTrue(
            all(
                right <= left + 1e-15
                for left, right in zip(rms_values, rms_values[1:])
            )
        )

    def test_three_paths_are_sample_identical(self) -> None:
        rng = np.random.default_rng(21)
        audio = AudioData(
            rng.normal(0.0, 0.08, (41000, 2)),
            sample_rate=22050,
            source_sha256="three-path-test",
        )
        config = RepairConfig(
            frame_size=512,
            hop_size=128,
            strength=1.4,
            noise_frames=5,
        )
        session_config = SessionConfig(
            segment_samples=9000,
            max_workers=3,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            realtime, realtime_result, _ = process_realtime_with_checkpoint(
                audio,
                config,
                session_config,
                root / "realtime",
                clear_checkpoint=True,
            )
            seed = RepairSession(
                audio,
                config,
                session_config,
                root / "resume",
                clear_checkpoint=True,
            )
            seed.run(max_new_segments=2)
            resumed = RepairSession(audio, config, session_config, root / "resume")
            self.assertGreater(resumed.load_info.loaded_count, 0)
            resumed_result = resumed.run()
            resume_output = resumed.collect_outputs()
        full = process_full_recompute(audio, config, session_config)
        self.assertTrue(realtime_result.complete)
        self.assertTrue(resumed_result.complete)
        self.assertTrue(np.array_equal(realtime, full))
        self.assertTrue(np.array_equal(resume_output, full))
        stats = [audio_stats(item, audio.sample_rate) for item in (realtime, resume_output, full)]
        self.assertEqual(stats[0].to_dict(), stats[1].to_dict())
        self.assertEqual(stats[1].to_dict(), stats[2].to_dict())

    def test_state_machine_rejects_illegal_transition_without_change(self) -> None:
        machine = SegmentStateMachine(split_segments(101, 40))
        machine.transition(0, SegmentStatus.PROCESSING)
        with self.assertRaises(AppError) as raised:
            machine.transition(0, SegmentStatus.PENDING)
        self.assertEqual(raised.exception.code, "E_STATE")
        self.assertEqual(machine.get(0).status, SegmentStatus.PROCESSING)
        machine.transition(0, SegmentStatus.FAILED)
        machine.retry(0)
        self.assertEqual(machine.get(0).status, SegmentStatus.PROCESSING)

    def test_parameter_and_decode_errors(self) -> None:
        with self.assertRaises(AppError) as strength:
            RepairConfig(strength=5.0).validate()
        self.assertEqual(strength.exception.code, "E_PARAM")
        with self.assertRaises(AppError) as workers:
            SessionConfig(max_workers=99).validate(2)
        self.assertEqual(workers.exception.code, "E_PARAM")
        corrupt = self.root / "corrupt.wav"
        corrupt.write_bytes(b"not a wave" * 32)
        with self.assertRaises(AppError) as decode:
            load_audio(corrupt)
        self.assertEqual(decode.exception.code, "E_DECODE")

    def test_version_mismatch_invalidates_entire_snapshot(self) -> None:
        audio = load_audio(self.sample_dir / "boundary_stereo_tail1.wav")
        config = default_config("music")
        session_config = SessionConfig(
            segment_samples=4096,
            max_workers=1,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        with tempfile.TemporaryDirectory() as temp:
            checkpoint = Path(temp) / "checkpoint"
            session = RepairSession(
                audio, config, session_config, checkpoint, clear_checkpoint=True
            )
            session.run(max_new_segments=2)
            manifest_path = checkpoint / "snapshot" / "manifest.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            manifest["schema_version"] = 999
            manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            resumed = RepairSession(audio, config, session_config, checkpoint)
            self.assertEqual(resumed.load_info.state, "invalidated")
            self.assertEqual(resumed.load_info.loaded_count, 0)
            self.assertTrue(
                all(
                    record.status == SegmentStatus.PENDING
                    for record in resumed.state_snapshot().values()
                )
            )
            self.assertIsNotNone(resumed.load_info.invalidated_to)
            self.assertTrue(Path(resumed.load_info.invalidated_to).exists())

    def test_failed_segment_can_retry_and_still_matches(self) -> None:
        audio = load_audio(self.sample_dir / "boundary_stereo_tail1.wav")
        config = default_config("music")
        session_config = SessionConfig(
            segment_samples=4096,
            max_workers=1,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        import audio_repair.engine as engine_module

        original = engine_module.process_segment
        failed_once = False

        def flaky(audio_arg: AudioData, spec: object, config_arg: RepairConfig) -> object:
            nonlocal failed_once
            if getattr(spec, "index") == 1 and not failed_once:
                failed_once = True
                raise RuntimeError("injected fault")
            return original(audio_arg, spec, config_arg)

        with tempfile.TemporaryDirectory() as temp:
            with mock.patch.object(engine_module, "process_segment", side_effect=flaky):
                session = RepairSession(
                    audio,
                    config,
                    session_config,
                    Path(temp) / "checkpoint",
                    clear_checkpoint=True,
                )
                first = session.run()
                self.assertEqual(first.failed_indices, (1,))
                with self.assertRaises(AppError) as state:
                    session.collect_outputs()
                self.assertEqual(state.exception.code, "E_STATE")
            second = session.run(retry_failed=True)
            output = session.collect_outputs()
        full = process_full_recompute(audio, config, session_config)
        self.assertTrue(second.complete)
        self.assertTrue(np.array_equal(output, full))

    def test_committed_output_survives_parameter_change(self) -> None:
        audio = load_audio(self.sample_dir / "speech_noisy_mono.wav")
        config = default_config("speech")
        session_config = SessionConfig(
            segment_samples=16384,
            max_workers=1,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        with tempfile.TemporaryDirectory() as temp:
            session = RepairSession(
                audio,
                config,
                session_config,
                Path(temp) / "checkpoint",
                clear_checkpoint=True,
            )
            session.run(max_new_segments=2)
            before = session.store.load_segment(0)
            session.set_repair_config(config.with_strength(0.5))
            session.run()
            after = session.store.load_segment(0)
        self.assertEqual(before.output_sha256, after.output_sha256)
        self.assertEqual(before.config.strength, after.config.strength)
        self.assertEqual(before.config_fingerprint, after.config_fingerprint)

    def test_checkpoint_commits_are_serial_and_ordered(self) -> None:
        rng = np.random.default_rng(31)
        audio = AudioData(
            rng.normal(0.0, 0.05, (20000, 1)),
            sample_rate=16000,
            source_sha256="commit-order-test",
        )
        config = RepairConfig(frame_size=256, hop_size=64, noise_frames=3)
        session_config = SessionConfig(
            segment_samples=4096,
            max_workers=4,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        with tempfile.TemporaryDirectory() as temp:
            session = RepairSession(
                audio,
                config,
                session_config,
                Path(temp) / "checkpoint",
                clear_checkpoint=True,
            )
            order: list[int] = []
            active = 0
            max_active = 0
            guard = threading.Lock()
            original = session.store.save_segment

            def tracked(
                spec: object,
                output: np.ndarray,
                config_arg: RepairConfig,
                dsp_details: object = None,
            ) -> str:
                nonlocal active, max_active
                with guard:
                    active += 1
                    max_active = max(max_active, active)
                try:
                    time.sleep(0.002)
                    digest = original(
                        spec, output, config_arg, dsp_details=dsp_details
                    )
                    with guard:
                        order.append(spec.index)
                    return digest
                finally:
                    with guard:
                        active -= 1

            session.store.save_segment = tracked  # type: ignore[method-assign]
            result = session.run()
        self.assertTrue(result.complete)
        self.assertEqual(max_active, 1)
        self.assertEqual(order, sorted(order))

    def test_first_difference_locates_segment(self) -> None:
        left = np.zeros((10000, 2), dtype=np.float64)
        right = np.array(left, copy=True)
        right[9003, 1] = 1e-9
        difference = locate_first_difference(left, right, 4096)
        self.assertFalse(difference.equal)
        self.assertEqual(difference.first_sample, 9003)
        self.assertEqual(difference.channel, 1)
        self.assertEqual(difference.segment_index, 2)


if __name__ == "__main__":
    unittest.main()

