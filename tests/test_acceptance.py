from __future__ import annotations

import dataclasses
import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from space_audio.analysis import FrameResult, analyze_frame
from space_audio.config import AnalysisConfig, RenderConfig, WorkbenchConfig
from space_audio.errors import ErrorCode, WorkbenchError
from space_audio.export import export_bundle
from space_audio.faults import FaultKind, inject_fault
from space_audio.generator import generate_dataset, generate_demo
from space_audio.loader import load_sample
from space_audio.model import frame_truth_for_sample
from space_audio.pipeline import (
    ProcessingSession,
    compare_results,
    run_full_recompute,
    run_realtime,
    run_resume,
)
from space_audio.render import render_audio
from space_audio.state import FrameState, FrameStateMachine


class AcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="spatial-audio-tests-")
        cls.root = Path(cls.temp.name)
        generated = generate_demo(
            cls.root / "sample",
            seed=20261123,
            frame_len=2048,
            hop=1024,
            window_len=768,
            frame_count=6,
        )
        cls.generated = generated
        cls.sample = load_sample(generated.manifest_path)
        cls.config = WorkbenchConfig(
            analysis=AnalysisConfig(),
            render=RenderConfig(),
            max_workers=2,
        )
        cls.realtime = run_realtime(
            cls.sample,
            cls.config,
            cls.root / "realtime.sqlite",
            fresh=True,
        )
        cls.partial = run_realtime(
            cls.sample,
            cls.config,
            cls.root / "resume.sqlite",
            stop_after_commits=2,
            fresh=True,
        )
        cls.resumed = run_resume(
            cls.sample,
            cls.config,
            cls.root / "resume.sqlite",
        )
        cls.recomputed = run_full_recompute(
            cls.sample,
            cls.root / "resume.sqlite",
            cls.config,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def assert_error_code(self, code: ErrorCode, callable_obj):
        with self.assertRaises(WorkbenchError) as context:
            callable_obj()
        self.assertEqual(context.exception.code, code)
        return context.exception

    def test_generator_truth_is_analytic_and_bounds_pass(self) -> None:
        previous = None
        frame_count = self.sample.frame_count(self.config.analysis)
        self.assertGreater(frame_count, 0)
        for frame_index in range(frame_count):
            truths = frame_truth_for_sample(
                self.sample,
                frame_index,
                self.config.analysis,
            )
            self.assertEqual(len(truths), self.sample.channel_count)
            result = analyze_frame(
                self.sample,
                frame_index,
                self.config,
                previous_cumulative_errors=previous,
            )
            previous = result.cumulative_errors
            self.assertEqual(len(result.channels), self.sample.channel_count)
            for estimate, truth in zip(result.channels, truths):
                self.assertLessEqual(
                    estimate.observed_errors.peak_quantization_s,
                    result.bounds.peak_quantization_s + 1.0e-12,
                )
                self.assertLessEqual(
                    estimate.observed_errors.energy_truncation_relative,
                    result.bounds.energy_truncation_relative + 1.0e-12,
                )
                self.assertLessEqual(
                    estimate.observed_errors.tdoa_s,
                    result.bounds.tdoa_s + 1.0e-12,
                )
                self.assertAlmostEqual(
                    estimate.direct_delay_samples,
                    truth.direct_delay_samples,
                    delta=self.sample.sample_rate * result.bounds.peak_quantization_s,
                )
                self.assertGreater(truth.reverb_energy, 0.0)
                self.assertTrue(np.isfinite(estimate.drr))

    def test_fixed_seed_is_reproducible(self) -> None:
        second = generate_demo(
            self.root / "repro",
            seed=20261123,
            frame_len=2048,
            hop=1024,
            window_len=768,
            frame_count=3,
        )
        first_three = self.sample.ir[:, : 3 * 1024]
        repeated = load_sample(second.manifest_path)
        self.assertTrue(
            np.array_equal(
                first_three,
                repeated.ir[:, : first_three.shape[1]],
            )
        )
        self.assertTrue(np.array_equal(self.sample.dry, repeated.dry))

    def test_multi_sample_dataset_manifest_is_selectable(self) -> None:
        dataset = generate_dataset(
            self.root / "dataset",
            seed=314159,
            count=2,
            layouts=("stereo", "quad"),
            frame_count=3,
        )
        self.assertEqual(len(dataset.sample_ids), 2)
        first = load_sample(dataset.manifest_path, sample_id=dataset.sample_ids[0])
        second = load_sample(dataset.manifest_path, sample_id=dataset.sample_ids[1])
        self.assertEqual(first.layout.name, "stereo")
        self.assertEqual(second.layout.name, "quad")
        self.assertEqual(first.channel_count, 2)
        self.assertEqual(second.channel_count, 4)

    def test_three_paths_are_sample_exact(self) -> None:
        self.assertTrue(self.realtime.report_complete)
        self.assertFalse(self.partial.report_complete)
        self.assertEqual(self.partial.committed_frames, 2)
        self.assertIsNone(self.partial.output)
        self.assertTrue(self.resumed.report_complete)
        self.assertTrue(self.recomputed.report_complete)
        self.assertIsNotNone(self.realtime.output)
        self.assertEqual(
            self.realtime.output.shape[1],
            self.sample.dry_length,
        )
        self.assertTrue(
            np.array_equal(self.realtime.output, self.resumed.output)
        )
        self.assertTrue(
            np.array_equal(self.realtime.output, self.recomputed.output)
        )
        summary_ab = compare_results(self.realtime, self.resumed)
        summary_ac = compare_results(self.realtime, self.recomputed)
        self.assertTrue(summary_ab.identical)
        self.assertTrue(summary_ac.identical)
        self.assertEqual(
            self.realtime.commit_sequence,
            self.resumed.commit_sequence,
        )
        self.assertEqual(
            self.realtime.commit_sequence,
            self.recomputed.commit_sequence,
        )

    def test_partial_run_cannot_export_complete_report(self) -> None:
        with self.assertRaises(ValueError):
            export_bundle(
                self.partial,
                self.root / "partial-export",
                sample_rate=self.sample.sample_rate,
                sample=self.sample,
            )

    def test_state_machine_rejects_illegal_transition_without_mutation(self) -> None:
        result = analyze_frame(self.sample, 0, self.config)
        machine = FrameStateMachine(4)
        with self.assertRaises(WorkbenchError) as context:
            machine.transition(0, FrameState.COMMITTED)
        self.assertEqual(context.exception.code, ErrorCode.STATE)
        self.assertEqual(machine.state(0), FrameState.PENDING)
        self.assertIsNone(machine.pending_result(0))

        machine.mark_estimated(0, result)
        frozen = machine.commit(0)
        payload = frozen.to_dict()
        with self.assertRaises(WorkbenchError) as context:
            machine.mark_estimated(0, result)
        self.assertEqual(context.exception.code, ErrorCode.STATE)
        self.assertEqual(machine.state(0), FrameState.COMMITTED)
        self.assertEqual(machine.committed_results[0].to_dict(), payload)
        self.assertEqual(machine.progress, (1, 4))

    def test_failed_frame_can_retry_without_polluting_committed(self) -> None:
        faulted = inject_fault(
            self.sample,
            FaultKind.BAD_FRAME,
            channel="R",
            frame_index=2,
            config=self.config,
        )
        session = ProcessingSession(faulted, self.config)
        session.process_frame(0)
        session.process_frame(1)
        committed_before = tuple(
            frame.to_dict() for frame in session.committed_results
        )
        with self.assertRaises(WorkbenchError) as context:
            session.process_frame(2)
        self.assertEqual(context.exception.code, ErrorCode.FRAME)
        self.assertEqual(session.state_machine.state(2), FrameState.FAILED)
        self.assertEqual(
            tuple(frame.to_dict() for frame in session.committed_results),
            committed_before,
        )
        session.retry_frame(2)
        self.assertEqual(session.state_machine.state(2), FrameState.PENDING)
        self.assertEqual(
            tuple(frame.to_dict() for frame in session.committed_results),
            committed_before,
        )
        session.close()

    def test_reconfigure_does_not_rewrite_committed_frames(self) -> None:
        session = ProcessingSession(self.sample, self.config)
        session.process_frame(0)
        session.process_frame(1)
        before = tuple(frame.to_dict() for frame in session.committed_results)
        changed = dataclasses.replace(
            self.config,
            analysis=dataclasses.replace(
                self.config.analysis,
                energy_threshold_db=-70.0,
            ),
        )
        session.reconfigure(changed)
        self.assertEqual(
            tuple(frame.to_dict() for frame in session.committed_results),
            before,
        )
        self.assertEqual(session.state_machine.state(0), FrameState.COMMITTED)
        self.assertEqual(session.state_machine.state(2), FrameState.PENDING)
        session.close()

    def test_frame_geometry_reconfigure_preserves_committed_prefix(self) -> None:
        session = ProcessingSession(self.sample, self.config)
        session.process_frame(0)
        session.process_frame(1)
        before = tuple(frame.to_dict() for frame in session.committed_results)
        changed = dataclasses.replace(
            self.config,
            analysis=AnalysisConfig(
                frame_len=1536,
                hop=768,
                window_len=512,
                interpolation_order=self.config.analysis.interpolation_order,
                energy_threshold_db=self.config.analysis.energy_threshold_db,
            ),
            render=RenderConfig(
                conv_window_len=self.config.render.conv_window_len,
                block_size=self.config.render.block_size,
                interpolation_order=self.config.analysis.interpolation_order,
            ),
        )
        session.reconfigure(changed)
        self.assertEqual(
            tuple(frame.to_dict() for frame in session.committed_results),
            before,
        )
        for frame_index in range(2, session.total_frames):
            session.process_frame(frame_index)
        self.assertEqual(session.progress, (session.total_frames, session.total_frames))
        self.assertEqual(
            tuple(frame.to_dict() for frame in session.committed_results[:2]),
            before,
        )
        session.close()

    def test_checkpoint_hash_mismatch_invalidates_whole_snapshot(self) -> None:
        checkpoint = self.root / "hash-invalid.sqlite"
        partial = run_realtime(
            self.sample,
            self.config,
            checkpoint,
            stop_after_commits=2,
            fresh=True,
        )
        self.assertEqual(partial.committed_frames, 2)
        changed_dry = self.sample.dry.copy()
        changed_dry[0, 0] += 1.0e-4
        changed = dataclasses.replace(self.sample, dry=changed_dry)
        resumed = run_resume(changed, self.config, checkpoint)
        self.assertTrue(resumed.invalidated)
        self.assertTrue(resumed.report_complete)
        self.assertEqual(
            resumed.committed_frames,
            resumed.total_frames,
        )
        self.assertEqual(resumed.commit_sequence, tuple(range(resumed.total_frames)))

    def test_algorithm_version_mismatch_invalidates_snapshot(self) -> None:
        checkpoint = self.root / "version-invalid.sqlite"
        run_realtime(
            self.sample,
            self.config,
            checkpoint,
            stop_after_commits=2,
            fresh=True,
        )
        changed = dataclasses.replace(
            self.config,
            algorithm_version="spatial-workbench-test-v2",
        )
        resumed = run_resume(self.sample, changed, checkpoint)
        self.assertTrue(resumed.invalidated)
        self.assertTrue(resumed.report_complete)
        self.assertEqual(resumed.algorithm_version, changed.algorithm_version)

    def test_error_injection_chains(self) -> None:
        cases = (
            (FaultKind.CHANNEL_MISSING, ErrorCode.CHAN),
            (FaultKind.TIMING_INVERSION, ErrorCode.ORDER),
            (FaultKind.GAIN_OVERFLOW, ErrorCode.CLIP),
            (FaultKind.BAD_FRAME, ErrorCode.FRAME),
            (FaultKind.BOUND_VIOLATION, ErrorCode.BOUND),
        )
        for kind, expected in cases:
            with self.subTest(kind=kind.value):
                faulted = inject_fault(
                    self.sample,
                    kind,
                    channel="R",
                    frame_index=2,
                    config=self.config,
                )
                if kind == FaultKind.CHANNEL_MISSING:
                    error = self.assert_error_code(
                        ErrorCode.CHAN,
                        lambda: run_realtime(
                            faulted,
                            self.config,
                            self.root / f"fault-{kind.value}.sqlite",
                            fresh=True,
                        ),
                    )
                    self.assertEqual(error.field, "ir")
                    continue
                if kind == FaultKind.TIMING_INVERSION:
                    error = self.assert_error_code(
                        ErrorCode.ORDER,
                        lambda: run_realtime(
                            faulted,
                            self.config,
                            self.root / f"fault-{kind.value}.sqlite",
                            fresh=True,
                        ),
                    )
                    self.assertEqual(error.channel, "R")
                    self.assertEqual(error.frame_index, 2)
                    continue
                result = run_realtime(
                    faulted,
                    self.config,
                    self.root / f"fault-{kind.value}.sqlite",
                    fresh=True,
                )
                self.assertFalse(result.report_complete)
                self.assertGreaterEqual(len(result.frame_errors), 1)
                error = result.frame_errors[0]
                self.assertEqual(error.code, expected)
                self.assertEqual(error.channel, "R")
                if kind == FaultKind.TIMING_INVERSION:
                    self.assertEqual(error.frame_index, 2)
                if kind == FaultKind.GAIN_OVERFLOW:
                    self.assertEqual(error.frame_index, 2)
                if kind == FaultKind.BAD_FRAME:
                    self.assertEqual(error.frame_index, 2)
                if kind == FaultKind.BOUND_VIOLATION:
                    self.assertEqual(error.frame_index, 2)
                    self.assertIsNotNone(error.excess)

    def test_metadata_errors_name_missing_field(self) -> None:
        original = json.loads(
            self.generated.manifest_path.read_text(encoding="utf-8")
        )
        for field_name in ("sample_rate", "channel_layout"):
            with self.subTest(field=field_name):
                payload = json.loads(json.dumps(original))
                del payload["samples"][0][field_name]
                manifest = self.generated.manifest_path.parent / f"missing_{field_name}.json"
                manifest.write_text(
                    json.dumps(payload, ensure_ascii=False, indent=2),
                    encoding="utf-8",
                )
                error = self.assert_error_code(
                    ErrorCode.META,
                    lambda manifest=manifest: load_sample(manifest),
                )
                self.assertIn(field_name, error.field or "")

    def test_parameter_channel_and_render_mapping_errors(self) -> None:
        bad = dataclasses.replace(
            self.config,
            analysis=dataclasses.replace(self.config.analysis, frame_len=10),
        )
        error = self.assert_error_code(ErrorCode.PARA, bad.validate)
        self.assertEqual(error.field, "frame_len")

        frames = self.realtime.frames
        bad_mapping = dataclasses.replace(
            self.config,
            render=dataclasses.replace(
                self.config.render,
                channel_map=(0, 1),
            ),
        )
        error = self.assert_error_code(
            ErrorCode.CHAN,
            lambda: render_audio(self.sample, frames, bad_mapping),
        )
        self.assertEqual(error.field, "channel_map")

        bad_index = dataclasses.replace(
            self.config,
            render=dataclasses.replace(
                self.config.render,
                channel_map=(0, 1, 2, 99),
            ),
        )
        error = self.assert_error_code(
            ErrorCode.PARA,
            lambda: render_audio(self.sample, frames, bad_index),
        )
        self.assertEqual(error.field, "channel_map")

    def test_first_divergence_reports_sample_and_frame(self) -> None:
        self.assertIsNotNone(self.realtime.render)
        output = self.realtime.output.copy()
        sample_index = 17
        output[0, sample_index] += 1.0e-9
        damaged_render = dataclasses.replace(
            self.realtime.render,
            output=output,
        )
        damaged = dataclasses.replace(self.realtime, render=damaged_render)
        with self.assertRaises(WorkbenchError) as context:
            compare_results(self.realtime, damaged)
        error = context.exception
        self.assertEqual(error.code, ErrorCode.DIVERGE)
        self.assertEqual(error.sample_index, sample_index)
        self.assertEqual(
            error.frame_index,
            sample_index // self.config.analysis.hop,
        )
        self.assertIn("path_a_value", error.details)
        self.assertIn("path_b_value", error.details)

    def test_output_has_exact_dry_duration_without_extension(self) -> None:
        self.assertEqual(self.realtime.output.shape[1], self.sample.dry.shape[1])
        rerendered = render_audio(
            self.sample,
            self.realtime.frames,
            self.config,
        )
        self.assertEqual(rerendered.output.shape[1], self.sample.dry_length)
        self.assertTrue(np.array_equal(rerendered.output, self.realtime.output))

    def test_parallel_and_serial_execution_render_identically(self) -> None:
        serial_config = dataclasses.replace(self.config, max_workers=1)
        serial = run_realtime(
            self.sample,
            serial_config,
            self.root / "serial.sqlite",
            fresh=True,
        )
        self.assertTrue(serial.report_complete)
        self.assertTrue(np.array_equal(serial.output, self.realtime.output))
        summary = compare_results(serial, self.realtime)
        self.assertTrue(summary.identical)

    def test_checkpoint_rejects_out_of_order_frame_write(self) -> None:
        from space_audio.checkpoint import CheckpointStore

        store = CheckpointStore(
            self.root / "ordered.sqlite",
            sample_id=self.sample.sample_id,
            input_hash=self.sample.input_hash(),
            algorithm_version=self.config.algorithm_version,
        )
        try:
            store.prepare(self.realtime.total_frames)
            self.assertGreaterEqual(len(self.realtime.frames), 2)
            with self.assertRaises(WorkbenchError) as context:
                store.save_frame(self.realtime.frames[1])
            self.assertEqual(context.exception.code, ErrorCode.STATE)
            store.save_frame(self.realtime.frames[0])
            self.assertEqual(
                store.snapshot_meta()["committed_frames"],
                "1",
            )
        finally:
            store.close()


if __name__ == "__main__":
    unittest.main()
