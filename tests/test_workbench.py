from __future__ import annotations

import hashlib
import json
import struct
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from loudness_workbench.config import (
    MeterConfig,
    SchedulerConfig,
    load_workbench_config,
)
from loudness_workbench.errors import ErrorCode, MeterError
from loudness_workbench.generator import generate_samples
from loudness_workbench.metering import (
    AssetMeter,
    analyze_offline,
    compare_reports,
)
from loudness_workbench.models import canonical_json
from loudness_workbench.playback import PlaybackController, PlaybackState
from loudness_workbench.scheduler import MeterScheduler
from loudness_workbench.snapshot import RecoveryManager, SnapshotStore
from loudness_workbench.wav import ChannelLayout, WaveAsset, downmix, parse_wav


def high_budget_scheduler(
    *,
    parallelism: int = 2,
    per_task: int = 128 * 1024 * 1024,
    global_budget: int = 256 * 1024 * 1024,
    oversample_batch: int = 1024,
    internal_buffer: int = 4096,
) -> SchedulerConfig:
    return SchedulerConfig(
        parallelism=parallelism,
        memory_budget_bytes=global_budget,
        per_task_memory_budget_bytes=per_task,
        oversample_batch_samples=oversample_batch,
        internal_buffer_samples=internal_buffer,
        degrade_sequence=(
            "increase_oversample_batch",
            "shrink_internal_buffer",
        ),
    )


class WorkbenchTestBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory(prefix="loudness-tests-")
        cls.root = Path(cls._temp.name)
        cls.sample_dir = cls.root / "samples"
        generate_samples(
            cls.sample_dir,
            seed=20261121,
            config_path=cls.sample_dir / "config.json",
        )
        cls.generated_config = load_workbench_config(
            cls.sample_dir / "config.json"
        )
        cls.meter_config = replace(
            cls.generated_config.meter,
            scheduler=high_budget_scheduler(),
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def wav(self, asset_id: str) -> Path:
        mapping = {
            "program_a_5_1": "program_a_5_1_24bit.wav",
            "program_b_stereo": "program_b_stereo_16bit.wav",
            "program_c_mono": "program_c_mono_16bit.wav",
        }
        return self.sample_dir / mapping[asset_id]

    def meter(
        self,
        asset_id: str = "program_b_stereo",
        *,
        config: MeterConfig | None = None,
        enforce_budget: bool = False,
    ) -> AssetMeter:
        return AssetMeter(
            str(self.wav(asset_id)),
            config=config or self.meter_config,
            asset_id=asset_id,
            enforce_budget=enforce_budget,
        )


class WavAndLayoutTests(WorkbenchTestBase):
    def test_16_and_24_bit_layout_and_downmix(self) -> None:
        info24 = parse_wav(self.wav("program_a_5_1"))
        self.assertEqual(info24.channels, 6)
        self.assertEqual(info24.bits_per_sample, 24)
        self.assertEqual(info24.sample_rate, 48_000)
        self.assertEqual(info24.layout, "5.1")
        self.assertEqual(info24.channel_mask, 0x3F)
        asset = WaveAsset(self.wav("program_a_5_1"))
        samples = asset.read_frames(0, 16)
        self.assertEqual(samples.shape, (16, 6))
        mixed = downmix(samples, info24)
        layout = ChannelLayout(
            info24.layout,
            info24.channel_mask,
            info24.labels,
        )
        self.assertEqual(mixed.weights, layout.weights)
        self.assertEqual(mixed.weights[3], 0.0)
        self.assertEqual(mixed.weights[4], 1.41)

        info16 = parse_wav(self.wav("program_b_stereo"))
        self.assertEqual(info16.bits_per_sample, 16)
        self.assertEqual(info16.layout, "stereo")
        self.assertEqual(info16.channel_mask, 0x0003)
        self.assertEqual(info16.channels, 2)

    def test_invalid_mask_and_sample_rate_return_channel_unsupported(self) -> None:
        original = self.wav("program_b_stereo").read_bytes()
        fmt_index = original.index(b"fmt ")
        payload = fmt_index + 8

        bad_mask = bytearray(original)
        bad_mask[payload + 20 : payload + 24] = struct.pack("<I", 0x00FF)
        bad_mask_path = self.root / "bad_mask.wav"
        bad_mask_path.write_bytes(bad_mask)
        with self.assertRaises(MeterError) as raised:
            parse_wav(bad_mask_path)
        self.assertEqual(raised.exception.code, ErrorCode.E_CHANNEL_UNSUPPORTED)

        bad_rate = bytearray(original)
        bad_rate[payload + 4 : payload + 8] = struct.pack("<I", 44_100)
        bad_rate_path = self.root / "bad_rate.wav"
        bad_rate_path.write_bytes(bad_rate)
        with self.assertRaises(MeterError) as raised:
            parse_wav(bad_rate_path)
        self.assertEqual(raised.exception.code, ErrorCode.E_CHANNEL_UNSUPPORTED)

    def test_unsupported_parameter_change_preserves_published_report(self) -> None:
        meter = self.meter()
        meter.process_all()
        before = meter.get_published()
        with self.assertRaises(MeterError) as raised:
            meter.update_params(replace(meter.config, sample_rate=44_100))
        self.assertEqual(raised.exception.code, ErrorCode.E_CHANNEL_UNSUPPORTED)
        after = meter.get_published()
        self.assertEqual(before.report_hash, after.report_hash)
        self.assertEqual(before.head_hash, after.head_hash)


class MeteringTests(WorkbenchTestBase):
    def test_online_and_offline_block_values_match(self) -> None:
        online_meter = self.meter()
        online = online_meter.process_all()
        offline = analyze_offline(
            str(self.wav("program_b_stereo")),
            config=self.meter_config,
            asset_id="program_b_stereo",
        )
        self.assertTrue(online.complete)
        self.assertTrue(offline.complete)
        self.assertEqual(online.block_count, offline.block_count)
        self.assertEqual(
            [block.sync_marker for block in online.blocks],
            [block.sync_marker for block in offline.blocks],
        )
        for actual, expected in zip(online.blocks, offline.blocks):
            self.assertEqual(actual.window_start, expected.window_start)
            self.assertEqual(actual.window_end, expected.window_end)
            self.assertEqual(actual.content_hash, expected.content_hash)
            self.assertAlmostEqual(
                actual.short_term_lufs,
                expected.short_term_lufs,
                places=8,
            )
            self.assertAlmostEqual(
                actual.integrated_lufs,
                expected.integrated_lufs,
                places=8,
            )
            self.assertAlmostEqual(
                actual.true_peak_dbtp,
                expected.true_peak_dbtp,
                places=8,
            )
        summary = compare_reports(online, offline, self.meter_config)
        self.assertEqual(summary["blocks_compared"], online.block_count)

    def test_mismatch_identifies_first_field_and_block(self) -> None:
        meter = self.meter()
        online = meter.process_all()
        offline = analyze_offline(
            str(self.wav("program_b_stereo")),
            config=self.meter_config,
            asset_id="program_b_stereo",
        )
        changed = list(offline.blocks)
        changed[2] = replace(
            changed[2],
            short_term_lufs=changed[2].short_term_lufs + 0.25,
        )
        bad_offline = replace(offline, blocks=tuple(changed))
        with self.assertRaises(MeterError) as raised:
            compare_reports(online, bad_offline, self.meter_config)
        error = raised.exception
        self.assertEqual(error.code, ErrorCode.E_LUFS_MISMATCH)
        self.assertEqual(error.context["block_index"], 2)
        self.assertEqual(error.context["field"], "short_term_lufs")
        self.assertEqual(error.context["sync_marker"], "program_b_stereo:000002")

    def test_incremental_reuses_raw_values_and_reslices(self) -> None:
        meter = self.meter()
        initial = meter.process_all()
        old_hashes = [block.content_hash for block in initial.blocks]
        old_lufs = initial.final_integrated_lufs
        queried: list[object] = []

        target_result = meter.update_params(
            replace(meter.config, target_lufs=-20.0)
        )
        self.assertEqual(target_result.affected_raw_blocks, ())
        self.assertEqual(len(target_result.reused_raw_blocks), initial.block_count)
        self.assertEqual(target_result.affected_derived_blocks, tuple(range(initial.block_count)))
        self.assertEqual(
            [block.content_hash for block in target_result.report.blocks],
            old_hashes,
        )
        self.assertTrue(all(block.target_lufs == -20.0 for block in target_result.report.blocks))
        self.assertEqual(
            target_result.parameter_version,
            initial.parameter_version + 1,
        )
        queried.append(meter.get_published())

        weights_result = meter.update_params(
            replace(meter.config, channel_weights=(0.8, 1.2))
        )
        self.assertEqual(
            weights_result.affected_raw_blocks,
            tuple(range(initial.block_count)),
        )
        self.assertEqual(weights_result.reused_raw_blocks, ())
        self.assertTrue(weights_result.report.complete)
        self.assertNotEqual(weights_result.report.final_integrated_lufs, old_lufs)
        queried.append(meter.get_published())

        reslice_result = meter.update_params(
            replace(meter.config, block_samples=2_400)
        )
        self.assertIsNotNone(reslice_result.reslice)
        self.assertEqual(
            reslice_result.reslice.old_block_samples,
            4_800,
        )
        self.assertEqual(
            reslice_result.reslice.new_block_samples,
            2_400,
        )
        self.assertGreater(len(reslice_result.reslice.added_boundaries), 0)
        self.assertEqual(
            len(reslice_result.affected_raw_blocks),
            reslice_result.report.block_count,
        )
        self.assertEqual(
            reslice_result.report.block_count,
            initial.block_count * 2,
        )
        queried.append(meter.get_published())
        self.assertEqual(len({id(item) for item in queried}), 3)


class SchedulerTests(WorkbenchTestBase):
    def test_stable_parallel_order_and_busy(self) -> None:
        trace_path = self.root / "trace-order.jsonl"
        first = self.meter("program_b_stereo")
        second = self.meter("program_c_mono")
        scheduler = MeterScheduler(self.meter_config, trace_path=trace_path)
        scheduler.start_asset(
            "program_b_stereo",
            first,
            block_indices=range(0, 3),
        )
        scheduler.start_asset(
            "program_c_mono",
            second,
            block_indices=range(0, 3),
        )
        with self.assertRaises(MeterError) as raised:
            scheduler.start_asset("program_b_stereo", first)
        self.assertEqual(raised.exception.code, ErrorCode.E_BUSY)
        result = scheduler.execute()
        self.assertTrue(result.ok)
        self.assertEqual(len(result.completions), 6)
        order = [(block.asset_id, block.block_index) for block in result.completions]
        self.assertEqual(
            order,
            [
                ("program_b_stereo", 0),
                ("program_c_mono", 0),
                ("program_b_stereo", 1),
                ("program_c_mono", 1),
                ("program_b_stereo", 2),
                ("program_c_mono", 2),
            ],
        )
        lines = [
            json.loads(line)
            for line in trace_path.read_text(encoding="utf-8").splitlines()
        ]
        self.assertEqual(lines[0]["type"], "trace_meta")
        self.assertEqual(lines[-1]["type"], "trace_end")
        previous = "0" * 64
        for event in lines:
            payload = dict(event)
            claimed = payload.pop("event_hash")
            actual = hashlib.sha256(
                canonical_json(payload).encode("utf-8")
            ).hexdigest()
            self.assertEqual(claimed, actual)
            self.assertEqual(payload["prev_hash"], previous)
            previous = claimed

    def test_budget_degrade_is_transparent_and_exceeded_is_localized(self) -> None:
        constrained_scheduler = replace(
            self.meter_config.scheduler,
            parallelism=1,
            memory_budget_bytes=20 * 1024 * 1024,
            per_task_memory_budget_bytes=9 * 1024 * 1024,
            oversample_batch_samples=512,
            internal_buffer_samples=65_536,
        )
        constrained = replace(self.meter_config, scheduler=constrained_scheduler)
        meter = self.meter(
            "program_a_5_1",
            config=constrained,
            enforce_budget=False,
        )
        scheduler = MeterScheduler(
            constrained,
            trace_path=self.root / "trace-degrade.jsonl",
        )
        scheduler.start_asset("program_a_5_1", meter, block_indices=[30])
        result = scheduler.execute()
        self.assertTrue(result.ok)
        target = result.completions[-1]
        self.assertEqual(target.block_index, 30)
        self.assertEqual(target.degrade_stage, "shrink_internal_buffer")
        self.assertGreater(result.waiting_events, 0)
        self.assertLessEqual(
            target.memory_bytes,
            constrained_scheduler.per_task_memory_budget_bytes,
        )

        reference = self.meter("program_a_5_1")
        reference_block = reference.process_block(30)
        self.assertAlmostEqual(
            target.short_term_lufs,
            reference_block.short_term_lufs,
            places=10,
        )
        self.assertAlmostEqual(
            target.integrated_lufs,
            reference_block.integrated_lufs,
            places=10,
        )
        self.assertAlmostEqual(
            target.true_peak_dbtp,
            reference_block.true_peak_dbtp,
            places=10,
        )

        tiny_scheduler = replace(
            constrained_scheduler,
            memory_budget_bytes=8 * 1024 * 1024,
            per_task_memory_budget_bytes=5 * 1024 * 1024,
        )
        tiny = replace(self.meter_config, scheduler=tiny_scheduler)
        tiny_meter = self.meter("program_a_5_1", config=tiny)
        rejected = MeterScheduler(
            tiny,
            trace_path=self.root / "trace-reject.jsonl",
        )
        rejected.start_asset(
            "program_a_5_1",
            tiny_meter,
            block_indices=[30],
        )
        rejected_result = rejected.execute()
        self.assertFalse(rejected_result.ok)
        self.assertEqual(
            rejected_result.errors[0]["code"],
            ErrorCode.E_BUDGET_EXCEEDED,
        )
        self.assertEqual(
            rejected_result.errors[0]["context"]["asset_id"],
            "program_a_5_1",
        )
        self.assertEqual(
            rejected_result.errors[0]["context"]["block_index"],
            30,
        )


class SnapshotAndPlaybackTests(WorkbenchTestBase):
    def test_snapshot_roundtrip_and_hash_mismatch(self) -> None:
        meter = self.meter()
        report = meter.process_all()
        path = self.root / "snapshot.json"
        store = SnapshotStore(path)
        store.save(report, expected_parameter_version=report.parameter_version)
        loaded = store.load(
            expected_parameter_version=report.parameter_version,
            expected_config_hash=report.config_hash,
        )
        self.assertEqual(loaded.report_hash, report.report_hash)
        self.assertEqual(loaded.head_hash, report.head_hash)
        with self.assertRaises(MeterError) as raised:
            store.load(expected_parameter_version=report.parameter_version + 1)
        self.assertEqual(raised.exception.code, ErrorCode.E_REPORT_MISMATCH)

        raw = json.loads(path.read_text(encoding="utf-8"))
        original_raw = json.loads(json.dumps(raw))
        raw["block_count"] += 1
        path.write_text(json.dumps(raw), encoding="utf-8")
        with self.assertRaises(MeterError) as raised:
            store.load()
        self.assertEqual(raised.exception.code, ErrorCode.E_REPORT_MISMATCH)

        raw = original_raw
        raw["blocks"][1]["short_term_lufs"] += 0.5
        path.write_text(json.dumps(raw), encoding="utf-8")
        with self.assertRaises(MeterError) as raised:
            store.load()
        self.assertEqual(raised.exception.code, ErrorCode.E_REPORT_MISMATCH)
        self.assertFalse(path.with_name(path.name + ".tmp").exists())

    def test_disk_only_recovery(self) -> None:
        meter = self.meter("program_c_mono")
        report = meter.process_all()
        manager = RecoveryManager(self.root / "recovery")
        path = manager.save(report)
        self.assertTrue(path.exists())
        recovered = manager.recover("program_c_mono")
        self.assertEqual(recovered.report_hash, report.report_hash)
        self.assertEqual(recovered.block_count, report.block_count)

    def test_play_pause_seek_restart_and_snapshot_recovery(self) -> None:
        meter = self.meter("program_c_mono")
        controller = PlaybackController(meter)
        self.assertEqual(controller.play(), PlaybackState.PLAYING)
        first = controller.advance(3)
        self.assertIsNotNone(first)
        self.assertEqual(controller.cursor_block, 3)
        self.assertEqual(controller.published.block_count, 3)
        controller.pause()
        self.assertEqual(controller.state, PlaybackState.PAUSED)

        sought = controller.seek_to_block(8)
        self.assertEqual(sought.block_index, 8)
        self.assertEqual(controller.cursor_block, 9)
        self.assertEqual(controller.published.block_count, 9)
        offline = analyze_offline(
            str(self.wav("program_c_mono")),
            config=self.meter_config,
            asset_id="program_c_mono",
        )
        self.assertAlmostEqual(
            sought.short_term_lufs,
            offline.blocks[8].short_term_lufs,
            places=8,
        )
        backward = controller.seek_to_block(3)
        self.assertEqual(backward.block_index, 3)
        self.assertEqual(controller.published.block_count, 4)
        self.assertEqual(controller.cursor_block, 4)
        controller.restart()
        self.assertEqual(controller.cursor_block, 0)
        self.assertEqual(controller.published.block_count, 0)
        self.assertEqual(controller.position_samples, 0)

        manager = RecoveryManager(self.root / "playback-recovery")
        manager.save(offline)
        recovered = manager.recover("program_c_mono")
        self.assertEqual(recovered.blocks[8].sync_marker, "program_c_mono:000008")


class DeterminismAndConfigTests(WorkbenchTestBase):
    def test_seeded_generation_and_trace_are_byte_deterministic(self) -> None:
        first_dir = self.root / "det-a"
        second_dir = self.root / "det-b"
        generate_samples(first_dir, seed=77, config_path=first_dir / "config.json")
        generate_samples(second_dir, seed=77, config_path=second_dir / "config.json")
        for filename in (
            "program_a_5_1_24bit.wav",
            "program_b_stereo_16bit.wav",
            "program_c_mono_16bit.wav",
            "truth.json",
        ):
            self.assertEqual(
                (first_dir / filename).read_bytes(),
                (second_dir / filename).read_bytes(),
            )

        traces = []
        for index in range(2):
            path = self.root / f"det-trace-{index}.jsonl"
            meter = self.meter("program_b_stereo")
            scheduler = MeterScheduler(self.meter_config, trace_path=path)
            scheduler.start_asset(
                "program_b_stereo",
                meter,
                block_indices=range(0, 3),
            )
            scheduler.execute()
            traces.append(path.read_bytes())
        self.assertEqual(traces[0], traces[1])

    def test_incomplete_config_is_rejected(self) -> None:
        path = self.root / "incomplete.json"
        payload = json.loads(
            (self.sample_dir / "config.json").read_text(encoding="utf-8")
        )
        del payload["meter"]["scheduler"]["degrade_sequence"]
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(MeterError) as raised:
            load_workbench_config(path)
        self.assertEqual(raised.exception.code, ErrorCode.E_CONFIG_INVALID)

    def test_generated_truth_matches_measurement(self) -> None:
        truth = json.loads(
            (self.sample_dir / "truth.json").read_text(encoding="utf-8")
        )
        by_id = {item["asset_id"]: item for item in truth["assets"]}
        meter = self.meter("program_a_5_1")
        report = meter.process_all()
        declared = by_id["program_a_5_1"]
        self.assertAlmostEqual(
            report.final_integrated_lufs,
            declared["declared_lufs"],
            delta=0.01,
        )
        maximum_peak = max(block.true_peak_dbtp for block in report.blocks)
        self.assertAlmostEqual(
            maximum_peak,
            declared["declared_true_peak_dbtp"],
            delta=0.05,
        )


if __name__ == "__main__":
    unittest.main()
