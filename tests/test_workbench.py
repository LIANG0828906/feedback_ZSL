from __future__ import annotations

import copy
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

import numpy as np

from imq_workbench.checkpoint import CheckpointStore
from imq_workbench.compression import (
    BlockOutcome,
    BlockProcessor,
    CompressionConfig,
    block_input_hash,
)
from imq_workbench.errors import WorkbenchError
from imq_workbench.metrics import evaluate_whole, heatmap_png_array, squared_error_map
from imq_workbench.pipeline import compare_pipeline_results, run_pipeline
from imq_workbench.pngio import load_image, read_png, write_png
from imq_workbench.report import export_quality_report
from imq_workbench.samples import generate_samples, load_manifest
from imq_workbench.state import BlockStateMachine, COMMITTED, FAILED, PENDING, PROCESSING
from imq_workbench.util import array_content_hash


ROOT = Path(__file__).resolve().parents[1]


class FaultyProcessor(BlockProcessor):
    def __init__(self, fail_index: int) -> None:
        self.fail_index = fail_index

    def process_block(self, image, truth_edge_mask, geometry, config, **kwargs):
        if geometry.index == self.fail_index:
            return BlockOutcome(
                index=geometry.index,
                geometry=geometry,
                state=FAILED,
                param={"block_size": config.block_size, "quant_step": config.quant_step},
                input_hash=block_input_hash(image, geometry),
                error={"status": "error", "code": "E_BLOCK", "message": "injected test failure", "details": {"block_index": geometry.index}},
            )
        return super().process_block(image, truth_edge_mask, geometry, config, **kwargs)


class WorkbenchTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="imq-tests-")
        cls.root = Path(cls.temp.name)
        cls.samples = cls.root / "samples"
        cls.manifest = generate_samples(cls.samples, preset="quick")
        cls.entries = {item["name"]: item for item in cls.manifest["samples"]}

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _load(self, name: str = "quick_gradient_gray") -> tuple[np.ndarray, np.ndarray, dict]:
        entry = self.entries[name]
        image = load_image(self.samples / entry["image_path"])
        with np.load(self.samples / entry["truth_path"], allow_pickle=False) as truth:
            mask = truth["edge_mask"].astype(bool, copy=False)
        return image, mask, entry

    def _run(
        self,
        name: str,
        *,
        db: str,
        mode: str = "realtime",
        block_size: int = 16,
        quant_step: float = 8.0,
        window: int = 7,
        workers: int = 2,
        **kwargs,
    ):
        image, mask, entry = self._load(name)
        config = CompressionConfig(
            block_size=block_size,
            quant_step=quant_step,
            evaluation_window=window,
            max_workers=workers,
        )
        return run_pipeline(
            image,
            mask,
            config,
            checkpoint_path=self.root / db,
            image_key=name,
            mode=mode,
            **kwargs,
        )

    def test_png_roundtrip_and_decode_error(self) -> None:
        gray = np.arange(48, dtype=np.uint8).reshape(6, 8)
        rgb = np.stack((gray, gray[::-1], (gray * 3) % 255), axis=2)
        gray_path = self.root / "roundtrip_gray.png"
        rgb_path = self.root / "roundtrip_rgb.png"
        write_png(gray_path, gray)
        write_png(rgb_path, rgb)
        np.testing.assert_array_equal(read_png(gray_path), gray)
        np.testing.assert_array_equal(read_png(rgb_path), rgb)
        broken = self.root / "broken.png"
        broken.write_bytes(b"\x89PNG\r\n\x1a\nnope")
        with self.assertRaises(WorkbenchError) as raised:
            load_image(broken)
        self.assertEqual(raised.exception.code, "E_DECODE")

    def test_sample_generator_is_deterministic(self) -> None:
        second_dir = self.root / "samples-second"
        second = generate_samples(second_dir, preset="quick", seed=self.manifest["seed"])
        first_hashes = [(item["name"], item["content_hash"]) for item in self.manifest["samples"]]
        second_hashes = [(item["name"], item["content_hash"]) for item in second["samples"]]
        self.assertEqual(first_hashes, second_hashes)

    def test_parameter_errors_include_field(self) -> None:
        with self.assertRaises(WorkbenchError) as block_error:
            CompressionConfig(block_size=12).validate()
        self.assertEqual(block_error.exception.code, "E_PARA")
        self.assertEqual(block_error.exception.details["field"], "block_size")
        with self.assertRaises(WorkbenchError) as quant_error:
            CompressionConfig(quant_step=0.0).validate()
        self.assertEqual(quant_error.exception.details["field"], "quant_step")
        with self.assertRaises(WorkbenchError) as band_error:
            CompressionConfig(block_size=8, evaluation_window=33).validate()
        self.assertEqual(band_error.exception.code, "E_BAND")

    def test_state_machine_illegal_transition_preserves_state(self) -> None:
        machine = BlockStateMachine(3)
        self.assertEqual(machine.transition(PROCESSING), PROCESSING)
        self.assertEqual(machine.transition(COMMITTED), COMMITTED)
        with self.assertRaises(WorkbenchError) as raised:
            machine.transition(PENDING)
        self.assertEqual(raised.exception.code, "E_STATE")
        self.assertEqual(machine.state, COMMITTED)

    def test_reconstruction_shape_and_edge_block_accounting(self) -> None:
        image, _, _ = self._load("quick_gradient_gray")
        result = self._run("quick_gradient_gray", db="shape.sqlite", block_size=16)
        self.assertTrue(result.complete)
        self.assertEqual(result.reconstructed.shape, image.shape)
        self.assertEqual(result.reconstructed.dtype, image.dtype)
        total_count = sum(entry["evaluation"]["pixel_count"] for entry in result.block_summaries.values())
        self.assertEqual(total_count, image.shape[0] * image.shape[1])
        expected_mse = float(np.mean((image.astype(np.float64) - result.reconstructed.astype(np.float64)) ** 2))
        self.assertAlmostEqual(result.metrics["mse"], expected_mse, places=12)
        np.testing.assert_allclose(
            result.heatmap,
            squared_error_map(image, result.reconstructed).astype(np.float32),
            rtol=0.0,
            atol=1e-6,
        )
        for index, summary in result.block_summaries.items():
            geometry = summary["geometry"]
            block_error = image[geometry["y0"] : geometry["y1"], geometry["x0"] : geometry["x1"]].astype(np.float64)
            reconstructed_error = result.reconstructed[geometry["y0"] : geometry["y1"], geometry["x0"] : geometry["x1"]].astype(np.float64)
            block_sse = float(np.sum((block_error - reconstructed_error) ** 2))
            self.assertAlmostEqual(summary["evaluation"]["pixel_sse"], block_sse, places=9)
            self.assertEqual(summary["evaluation"]["pixel_count"], block_error.size)

    def test_quant_step_monotonic(self) -> None:
        rows = []
        for quant_step in (1.0, 2.0, 4.0, 8.0, 16.0):
            result = self._run(
                "quick_mixed_rgb",
                db=f"monotonic-{quant_step}.sqlite",
                mode="recompute",
                block_size=16,
                quant_step=quant_step,
            )
            rows.append((result.metrics["mse"], result.metrics["psnr_db"]))
        for previous, current in zip(rows, rows[1:]):
            self.assertGreaterEqual(current[0], previous[0])
            self.assertLessEqual(current[1], previous[1])

    def test_blockwise_metrics_match_whole_image(self) -> None:
        image, mask, _ = self._load("quick_mixed_rgb")
        result = self._run("quick_mixed_rgb", db="whole.sqlite", block_size=16, window=9)
        whole = evaluate_whole(image, result.reconstructed, mask, evaluation_window=9)
        for key in ("mse", "psnr_db", "ssim", "edge_fidelity"):
            self.assertAlmostEqual(result.metrics[key], whole[key], places=9)
        self.assertEqual(result.metrics["truth_edge_pixels"], int(mask.sum()))

    def test_three_paths_are_pixel_and_metric_identical(self) -> None:
        db = "paths.sqlite"
        realtime = self._run("quick_gradient_gray", db=db, mode="realtime", workers=2)
        event = threading.Event()

        def pause_after(done: int, total: int, index: int, state: str) -> None:
            if state != "loaded" and done >= 12:
                event.set()

        partial = self._run(
            "quick_gradient_gray",
            db=db,
            mode="realtime",
            workers=2,
            cancel_event=event,
            progress_callback=pause_after,
        )
        self.assertFalse(partial.complete)
        self.assertGreater(partial.progress["committed"], 0)
        resumed = self._run("quick_gradient_gray", db=db, mode="resume", workers=4)
        recomputed = self._run("quick_gradient_gray", db=db, mode="recompute", workers=1)
        compare_pipeline_results(realtime, resumed)
        compare_pipeline_results(realtime, recomputed)
        comparison = compare_pipeline_results(resumed, recomputed)
        self.assertEqual(comparison["status"], "ok")
        self.assertEqual(resumed.resumed_blocks, partial.progress["committed"])

    def test_diff_locator_reports_first_pixel_and_block(self) -> None:
        first = self._run("quick_gradient_gray", db="diff-a.sqlite", block_size=16)
        second = self._run("quick_gradient_gray", db="diff-b.sqlite", mode="recompute", block_size=16)
        damaged = copy.copy(second)
        damaged.reconstructed = second.reconstructed.copy()
        y, x = 17, 18
        damaged.reconstructed[y, x] ^= 1
        with self.assertRaises(WorkbenchError) as raised:
            compare_pipeline_results(first, damaged)
        self.assertEqual(raised.exception.code, "E_DIFF")
        self.assertEqual(raised.exception.details["stage"], "reconstruction")
        self.assertEqual(raised.exception.details["first_pixel"], {"y": y, "x": x})
        self.assertEqual(raised.exception.details["block_index"], 1 * first.grid[1] + 1)

    def test_incomplete_report_is_rejected(self) -> None:
        event = threading.Event()

        def pause_after(done: int, total: int, index: int, state: str) -> None:
            if state != "loaded" and done >= 1:
                event.set()

        partial = self._run(
            "quick_gradient_gray",
            db="partial-report.sqlite",
            block_size=16,
            cancel_event=event,
            progress_callback=pause_after,
        )
        with self.assertRaises(WorkbenchError) as raised:
            export_quality_report(partial, self.root / "invalid-report")
        self.assertEqual(raised.exception.code, "E_STATE")
        self.assertFalse((self.root / "invalid-report" / "quality_report.json").exists())

    def test_report_export_and_heatmap(self) -> None:
        result = self._run("quick_gradient_gray", db="report.sqlite", block_size=16)
        output = self.root / "report"
        exported = export_quality_report(result, output)
        self.assertTrue(Path(exported["report_path"]).exists())
        self.assertTrue(Path(exported["heatmap_png"]).exists())
        self.assertTrue(Path(exported["reconstruction_png"]).exists())
        self.assertTrue(all(len(summary["input_hash"]) == 64 for summary in result.block_summaries.values()))
        heatmap = read_png(exported["heatmap_png"])
        self.assertEqual(heatmap.shape, result.heatmap.shape + (3,))
        self.assertEqual(heatmap_png_array(result.heatmap).shape, result.heatmap.shape + (3,))

    def test_failed_block_is_isolated_and_retry_completes(self) -> None:
        image, mask, _ = self._load("quick_gradient_gray")
        config = CompressionConfig(block_size=16, quant_step=8.0, evaluation_window=7, max_workers=3)
        db = self.root / "fault.sqlite"
        failed = run_pipeline(
            image,
            mask,
            config,
            checkpoint_path=db,
            image_key="fault",
            mode="realtime",
            processor=FaultyProcessor(fail_index=5),
        )
        self.assertFalse(failed.complete)
        self.assertEqual(failed.failed_blocks, [5])
        self.assertEqual(failed.block_states[5], FAILED)
        self.assertGreater(failed.progress["committed"], 0)
        retried = run_pipeline(
            image,
            mask,
            config,
            checkpoint_path=db,
            image_key="fault",
            mode="resume",
            only_indices={5},
        )
        self.assertTrue(retried.complete)
        clean = run_pipeline(
            image,
            mask,
            config,
            checkpoint_path=self.root / "fault-clean.sqlite",
            image_key="fault-clean",
            mode="recompute",
        )
        np.testing.assert_array_equal(clean.reconstructed, retried.reconstructed)
        self.assertEqual(clean.metrics, retried.metrics)
        self.assertEqual(sorted(retried.commit_order), clean.commit_order)

    def test_committed_blocks_are_frozen_when_quant_step_changes(self) -> None:
        db = "frozen.sqlite"

        def pause_after(done: int, total: int, index: int, state: str) -> None:
            if state != "loaded" and done >= 6:
                event.set()

        event = threading.Event()
        partial = self._run(
            "quick_gradient_gray",
            db=db,
            mode="realtime",
            quant_step=8.0,
            cancel_event=event,
            progress_callback=pause_after,
        )
        self.assertFalse(partial.complete)
        frozen_count = partial.progress["committed"]
        resumed = self._run("quick_gradient_gray", db=db, mode="resume", quant_step=3.0)
        for index in range(frozen_count):
            self.assertEqual(resumed.block_summaries[index]["params"]["quant_step"], 8.0)
        self.assertEqual(resumed.block_summaries[len(resumed.block_summaries) - 1]["params"]["quant_step"], 3.0)

    def test_snapshot_invalidates_on_image_hash_mismatch(self) -> None:
        db = "invalidate.sqlite"
        first = self._run("quick_gradient_gray", db=db, mode="realtime", block_size=16)
        image, mask, _ = self._load("quick_gradient_gray")
        changed = image.copy()
        changed[0, 0] ^= 1
        result = run_pipeline(
            changed,
            mask,
            CompressionConfig(block_size=16, quant_step=8.0, evaluation_window=7, max_workers=2),
            checkpoint_path=self.root / db,
            image_key="quick_gradient_gray",
            mode="resume",
        )
        self.assertEqual(result.checkpoint_invalidated_reason, "image_content_hash_mismatch")
        self.assertEqual(result.resumed_blocks, 0)
        self.assertFalse(np.array_equal(first.image, result.image))

    def test_snapshot_invalidates_on_algorithm_version_mismatch(self) -> None:
        db = "version.sqlite"
        first = self._run("quick_gradient_gray", db=db, mode="realtime", block_size=16)
        image, _, _ = self._load("quick_gradient_gray")
        store = CheckpointStore(self.root / db)
        try:
            loaded = store.load_compatible(
                image=image,
                image_key="quick_gradient_gray",
                image_hash=array_content_hash(image),
                block_size=16,
                shape=image.shape,
                algorithm_version="different-algorithm-version",
            )
        finally:
            store.close()
        self.assertFalse(loaded.valid)
        self.assertEqual(loaded.invalidated_reason, "algorithm_version_mismatch")
        self.assertEqual(loaded.blocks, {})

    def test_resume_rejects_block_size_change_after_commit(self) -> None:
        event = threading.Event()

        def pause_after(done: int, total: int, index: int, state: str) -> None:
            if state != "loaded" and done >= 2:
                event.set()

        partial = self._run(
            "quick_gradient_gray",
            db="partition.sqlite",
            mode="realtime",
            block_size=16,
            cancel_event=event,
            progress_callback=pause_after,
        )
        self.assertFalse(partial.complete)
        with self.assertRaises(WorkbenchError) as raised:
            self._run(
                "quick_gradient_gray",
                db="partition.sqlite",
                mode="resume",
                block_size=32,
            )
        self.assertEqual(raised.exception.code, "E_PARA")
        self.assertEqual(raised.exception.details["field"], "block_size")

    def test_corrupt_block_payload_invalidates_entire_snapshot(self) -> None:
        db = "payload.sqlite"
        first = self._run("quick_gradient_gray", db=db, mode="realtime", block_size=16)
        with sqlite3.connect(self.root / db) as connection:
            connection.execute(
                "UPDATE blocks SET stats_json=? WHERE snapshot_id=? AND block_index=0",
                (b'{"tampered":true}', first.snapshot_id),
            )
            connection.commit()
        resumed = self._run("quick_gradient_gray", db=db, mode="resume", block_size=16)
        self.assertEqual(resumed.checkpoint_invalidated_reason, "block_payload_validation_failed")
        self.assertEqual(resumed.resumed_blocks, 0)
        self.assertTrue(resumed.complete)

    def test_hard_termination_then_resume(self) -> None:
        image, _, _ = self._load("quick_gradient_gray")
        db = self.root / "crash.sqlite"
        command = [
            sys.executable,
            "-m",
            "imq_workbench",
            "run",
            "--samples-dir",
            str(self.samples),
            "--sample",
            "quick_gradient_gray",
            "--block-size",
            "16",
            "--quant-step",
            "8",
            "--eval-window",
            "7",
            "--workers",
            "1",
            "--checkpoint",
            str(db),
            "--mode",
            "realtime",
            "--crash-after",
            "5",
            "--no-report",
            "--json",
        ]
        crashed = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=30)
        self.assertEqual(crashed.returncode, 137)
        with sqlite3.connect(db) as connection:
            row = connection.execute("SELECT COUNT(*) FROM blocks").fetchone()
        committed = int(row[0])
        self.assertGreater(committed, 0)
        total = int(np.ceil(image.shape[0] / 16) * np.ceil(image.shape[1] / 16))
        self.assertLess(committed, total)
        self.assertFalse((self.root / "crash-report").exists())

        resume_command = [
            sys.executable,
            "-m",
            "imq_workbench",
            "run",
            "--samples-dir",
            str(self.samples),
            "--sample",
            "quick_gradient_gray",
            "--block-size",
            "16",
            "--quant-step",
            "8",
            "--eval-window",
            "7",
            "--workers",
            "3",
            "--checkpoint",
            str(db),
            "--mode",
            "resume",
            "--output-dir",
            str(self.root / "crash-report"),
            "--json",
        ]
        resumed = subprocess.run(resume_command, cwd=ROOT, capture_output=True, text=True, timeout=30)
        self.assertEqual(resumed.returncode, 0, resumed.stderr + resumed.stdout)
        payload = json.loads(resumed.stdout)
        self.assertTrue(payload["complete"])
        self.assertEqual(payload["resumed_blocks"], committed)
        self.assertTrue((self.root / "crash-report" / "quality_report.json").exists())

    def test_ui_offscreen_smoke(self) -> None:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
        from PySide6.QtWidgets import QApplication
        from imq_workbench.ui import WorkbenchWindow

        app = QApplication.instance() or QApplication([])
        window = WorkbenchWindow(self.samples, self.root / "ui.sqlite", self.root / "runs")
        self.assertGreater(window.sample_combo.count(), 0)
        self.assertIsNotNone(window.image)
        window._start("realtime")
        deadline = time.monotonic() + 15.0
        while window.worker is not None and time.monotonic() < deadline:
            app.processEvents()
            time.sleep(0.01)
        app.processEvents()
        self.assertIsNotNone(window.result)
        self.assertTrue(window.result.complete)
        self.assertTrue(window.export_button.isEnabled())
        window.close()


if __name__ == "__main__":
    unittest.main()
