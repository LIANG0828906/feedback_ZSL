from __future__ import annotations

from pathlib import Path
import tempfile
import unittest

import numpy as np

from edgeforge.comparison import first_stage_difference
from edgeforge.config import PipelineConfig
from edgeforge.errors import BandError, BudgetError, CancelledError, PipelineFailure
from edgeforge.loader import load_sample
from edgeforge.pipeline import Pipeline, process_whole_image
from edgeforge.samples import generate_samples
from edgeforge.stages import BlockState, Stage


class PipelineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name)
        generate_samples(cls.root / "samples", ["low_contrast_gray_320x240"])
        cls.sample = load_sample(
            cls.root / "samples",
            "low_contrast_gray_320x240",
        )
        cls.config = PipelineConfig(
            tile_size=64,
            max_parallel=4,
            memory_budget_bytes=16 * 1024 * 1024,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_serial_parallel_whole_and_resume_are_pixel_identical(self) -> None:
        serial = Pipeline(self.sample, self.config, execution_mode="serial").run()
        parallel = Pipeline(self.sample, self.config, execution_mode="parallel").run()
        whole = process_whole_image(self.sample, self.config)
        self.assertIsNone(
            first_stage_difference(
                serial.stage_arrays,
                parallel.stage_arrays,
                parallel.blocks,
                self.config.tile_size,
            )
        )
        self.assertIsNone(
            first_stage_difference(
                serial.stage_arrays,
                whole,
                serial.blocks,
                self.config.tile_size,
            )
        )
        self.assertEqual(serial.stage_metrics, parallel.stage_metrics)
        self.assertEqual(
            serial.progress["committed"],
            len(serial.blocks) * len(Stage),
        )

        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp) / "checkpoint"
            partial = Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            )
            partial.run(stages_limit=2)
            partial_progress = partial.progress
            resumed = Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            )
            self.assertGreater(resumed.resumed_commits, 0)
            resumed.run()
            self.assertIsNone(
                first_stage_difference(
                    serial.stage_arrays,
                    resumed.stage_arrays,
                    resumed.blocks,
                    self.config.tile_size,
                )
            )
            self.assertEqual(serial.stage_metrics, resumed.stage_metrics)
            self.assertEqual(
                partial_progress["committed"],
                resumed.resumed_commits,
            )

    def test_hash_mismatch_invalidates_entire_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp) / "checkpoint"
            Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            ).run(stages_limit=3)
            record = sorted((run_dir / "records").glob("*.npz"))[0]
            with np.load(record, allow_pickle=False) as data:
                payload = {key: np.array(data[key], copy=True) for key in data.files}
            array = payload["array"]
            if array.dtype == np.uint8:
                array.reshape(-1)[0] = np.uint8(1 - int(array.reshape(-1)[0]))
            else:
                array.reshape(-1)[0] = array.reshape(-1)[0] + np.array(
                    1.0,
                    dtype=array.dtype,
                )
            np.savez_compressed(record, **payload)
            recovered = Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            )
            self.assertTrue(recovered.snapshot_invalidated)
            self.assertEqual(recovered.resumed_commits, 0)
            recovered.run()
            serial = Pipeline(
                self.sample,
                self.config,
                execution_mode="serial",
            ).run()
            self.assertIsNone(
                first_stage_difference(
                    serial.stage_arrays,
                    recovered.stage_arrays,
                    recovered.blocks,
                    self.config.tile_size,
                )
            )

    def test_resume_from_partially_committed_stage(self) -> None:
        baseline = Pipeline(
            self.sample,
            self.config,
            execution_mode="serial",
        ).run()
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp) / "checkpoint"
            pipeline = Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            )
            committed = {"count": 0}

            def cancel_after_some_commits(event: dict[str, object]) -> None:
                if event.get("type") == "block_committed":
                    committed["count"] += 1
                    if committed["count"] == 25:
                        pipeline.control.cancel()

            pipeline.on_event = cancel_after_some_commits
            with self.assertRaises(CancelledError):
                pipeline.run()
            committed_before_exit = (
                pipeline.checkpoint.next_seq if pipeline.checkpoint else 0
            )
            self.assertGreater(committed_before_exit, 20)
            self.assertLess(committed_before_exit, 40)
            snapshot = pipeline.state_machine.snapshot()
            partial_stage_commits = [
                cell
                for (stage, _block_id), cell in snapshot.cells.items()
                if stage is Stage.THRESHOLD and cell.state is BlockState.COMMITTED
            ]
            self.assertGreater(len(partial_stage_commits), 0)
            self.assertLess(len(partial_stage_commits), len(pipeline.blocks))

            resumed = Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            )
            self.assertEqual(resumed.resumed_commits, committed_before_exit)
            resumed.run()
            self.assertIsNone(
                first_stage_difference(
                    baseline.stage_arrays,
                    resumed.stage_arrays,
                    resumed.blocks,
                    self.config.tile_size,
                )
            )

    def test_interrupted_atomic_temp_file_is_ignored_on_restart(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp) / "checkpoint"
            Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            ).run(stages_limit=2)
            (run_dir / "records" / ".tmp-crash.npz").write_bytes(b"partial")
            resumed = Pipeline(
                self.sample,
                self.config,
                execution_mode="parallel",
                run_dir=run_dir,
            )
            self.assertFalse(resumed.snapshot_invalidated)
            self.assertGreater(resumed.resumed_commits, 0)

    def test_single_block_failure_does_not_pollute_other_blocks(self) -> None:
        baseline = Pipeline(
            self.sample,
            self.config,
            execution_mode="serial",
        ).run()
        pipeline = Pipeline(
            self.sample,
            self.config,
            execution_mode="parallel",
            fault_once={(Stage.OPEN, 1)},
        )
        with self.assertRaises(PipelineFailure):
            pipeline.run()
        snapshot = pipeline.state_machine.snapshot()
        self.assertEqual(
            snapshot.cells[(Stage.OPEN, 1)].state,
            BlockState.FAILED,
        )
        open_states = [
            snapshot.cells[(Stage.OPEN, block.block_id)].state
            for block in pipeline.blocks
        ]
        self.assertEqual(open_states.count(BlockState.COMMITTED), len(pipeline.blocks) - 1)
        self.assertTrue(all(
            snapshot.cells[(Stage.THIN, block.block_id)].state
            is BlockState.PENDING
            for block in pipeline.blocks
        ))
        self.assertIn(Stage.THRESHOLD, pipeline.stage_arrays)
        self.assertNotIn(Stage.CLOSE, pipeline.stage_arrays)
        pipeline.run(retry_failed=True)
        self.assertIsNone(
            first_stage_difference(
                baseline.stage_arrays,
                pipeline.stage_arrays,
                pipeline.blocks,
                self.config.tile_size,
            )
        )

    def test_budget_queues_without_dropping_blocks(self) -> None:
        constrained = PipelineConfig(
            tile_size=128,
            max_parallel=4,
            memory_budget_bytes=200_000,
        )
        pipeline = Pipeline(
            self.sample,
            constrained,
            execution_mode="parallel",
        ).run()
        self.assertGreater(len(pipeline.queue_events), 0)
        self.assertEqual(
            pipeline.progress["committed"],
            len(pipeline.blocks) * len(Stage),
        )
        self.assertTrue(
            all(event["reason"] == "memory_budget" for event in pipeline.queue_events)
        )

    def test_insufficient_band_reports_block_and_width(self) -> None:
        config = PipelineConfig(tile_size=64, band_override=1)
        pipeline = Pipeline(self.sample, config, execution_mode="serial")
        with self.assertRaises(BandError) as context:
            pipeline.run()
        self.assertEqual(context.exception.code, "E_BAND")
        self.assertIsNotNone(context.exception.block_id)
        self.assertGreater(
            int(context.exception.details["required_width"]),
            int(context.exception.details["configured_width"]),
        )

    def test_thread_buffer_error_is_explicit(self) -> None:
        config = PipelineConfig(tile_size=256, thread_buffer_pixels=4096)
        pipeline = Pipeline(self.sample, config, execution_mode="parallel")
        with self.assertRaises(BudgetError) as context:
            pipeline.run()
        self.assertEqual(context.exception.code, "E_BUDGET")
        self.assertEqual(context.exception.field, "thread_buffer_pixels")

    def test_first_difference_locates_pixel_block_and_stage(self) -> None:
        serial = Pipeline(
            self.sample,
            self.config,
            execution_mode="serial",
        ).run()
        changed = {
            stage: np.array(array, copy=True)
            for stage, array in serial.stage_arrays.items()
        }
        changed[Stage.THIN][10, 65] = 1 - changed[Stage.THIN][10, 65]
        difference = first_stage_difference(
            serial.stage_arrays,
            changed,
            serial.blocks,
            self.config.tile_size,
        )
        self.assertIsNotNone(difference)
        self.assertEqual(difference["x"], 65)
        self.assertEqual(difference["y"], 10)
        self.assertEqual(difference["stage"], Stage.THIN.value)
        self.assertEqual(difference["block_id"], 1)


if __name__ == "__main__":
    unittest.main()

