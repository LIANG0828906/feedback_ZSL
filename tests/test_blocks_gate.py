from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from loudness_diag.batch import run_batch
from loudness_diag.blocks import analyze_asset, compare_slicing, slice_blocks
from loudness_diag.gate import GateParameters, reconstruct_gate_decisions
from loudness_diag.generator import generate_test_set
from loudness_diag.jsonutil import canonical_bytes
from loudness_diag.scheduler import SchedulerConfig
from loudness_diag.wav import read_wav


class BlockGateTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name)
        generate_test_set(cls.root, seed=2026, seconds=2.0)
        cls.wav = read_wav(cls.root / "program_51_16bit.wav")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _config(self, parallelism: int, blocks_in_memory: int = 1) -> SchedulerConfig:
        unit = 9600 * 6 * 16
        return SchedulerConfig(
            parallelism=parallelism,
            memory_budget_bytes=unit * blocks_in_memory,
            block_memory_bytes=unit,
        )

    def test_block_metrics_and_two_stage_gate_are_observable(self) -> None:
        report, trace = analyze_asset(
            self.wav,
            block_samples=9600,
            asset_id="gate-test",
            scheduler_config=self._config(2, 2),
        )
        self.assertEqual(len(report["blocks"]), 10)
        for index, row in enumerate(report["blocks"]):
            self.assertEqual(row["block_id"], f"{index:08d}")
            self.assertEqual(row["window_start"], index * 9600)
            self.assertEqual(row["window_end"], (index + 1) * 9600)
            self.assertIn(str(row["window_start"]), row["sync_marker"])
            self.assertGreaterEqual(row["ungated_energy"], 0.0)
            self.assertGreater(row["true_peak"], 0.0)
            self.assertIn("absolute_pass", row)
            self.assertIn("relative_pass", row)
            self.assertIn("removed_reason", row)
        removed = [
            row
            for row in report["blocks"]
            if row["removed_reason"] is not None
        ]
        self.assertTrue(removed)
        for row in removed:
            if row["removed_reason"] == "absolute_gate":
                self.assertFalse(row["absolute_pass"])
            elif row["removed_reason"] == "relative_gate":
                self.assertTrue(row["absolute_pass"])
                self.assertFalse(row["relative_pass"])

        rebuilt = reconstruct_gate_decisions(
            report["blocks"],
            absolute_threshold_energy=report["decision_index"][
                "absolute_threshold_energy"
            ],
            relative_threshold_energy=report["decision_index"][
                "relative_threshold_energy"
            ],
        )
        for row in report["blocks"]:
            self.assertEqual(
                rebuilt[row["block_id"]]["final_pass"], row["final_pass"]
            )
            if row["removed_reason"] in {"absolute_gate", "relative_gate"}:
                self.assertEqual(
                    rebuilt[row["block_id"]]["removed_reason"],
                    row["removed_reason"],
                )
        self.assertTrue(any(event["event"] == "wave_started" for event in trace))
        self.assertTrue(
            all(
                left["submit_seq"] < right["submit_seq"]
                for left, right in zip(
                    [event for event in trace if event["event"] == "task_queued"],
                    [event for event in trace if event["event"] == "task_queued"][1:],
                )
            )
        )

    def test_parallel_and_serial_blocks_are_equal_and_repeatable(self) -> None:
        serial, _serial_trace = analyze_asset(
            self.wav,
            block_samples=9600,
            asset_id="same",
            scheduler_config=self._config(1, 1),
        )
        parallel, parallel_trace = analyze_asset(
            self.wav,
            block_samples=9600,
            asset_id="same",
            scheduler_config=self._config(3, 2),
        )
        parallel_again, parallel_trace_again = analyze_asset(
            self.wav,
            block_samples=9600,
            asset_id="same",
            scheduler_config=self._config(3, 2),
        )
        self.assertEqual(serial["blocks"], parallel["blocks"])
        self.assertEqual(parallel, parallel_again)
        self.assertEqual(parallel_trace, parallel_trace_again)
        self.assertEqual(
            canonical_bytes(parallel), canonical_bytes(parallel_again)
        )
        memory_waits = [
            event
            for event in parallel_trace
            if event["event"] == "task_queued"
            and event["wait_reason"] == "memory_budget"
        ]
        self.assertTrue(memory_waits)
        self.assertEqual(
            parallel["analysis"]["scheduler"]["parallelism"],
            3,
        )

    def test_reslicing_reports_count_boundary_and_hash_mapping(self) -> None:
        comparison = compare_slicing(
            self.wav,
            old_block_samples=9600,
            new_block_samples=19200,
            old_oversample=2,
            new_oversample=4,
            asset_id="reslice",
            scheduler_config=self._config(1, 1),
        )
        self.assertEqual(comparison["old"]["block_count"], 10)
        self.assertEqual(comparison["new"]["block_count"], 5)
        self.assertEqual(comparison["block_count_delta"], -5)
        self.assertEqual(comparison["boundary_deviation_samples"], 9600)
        self.assertEqual(len(comparison["old_to_new_block_mapping"]), 10)
        self.assertTrue(
            any(
                not item["evaluation_hash_equal"]
                for items in comparison["old_to_new_block_mapping"].values()
                for item in items
            )
        )

    def test_partial_tail_block_is_marked_incomplete(self) -> None:
        specs = slice_blocks(self.wav, 10000, asset_id="tail")
        report, _ = analyze_asset(
            self.wav,
            block_samples=10000,
            asset_id="tail",
            scheduler_config=self._config(1, 1),
        )
        self.assertEqual(specs[-1].sample_count, 6000)
        self.assertFalse(report["blocks"][-1]["complete"])
        self.assertTrue(all(row["complete"] for row in report["blocks"][:-1]))

    def test_batch_is_deterministic_and_marks_flips(self) -> None:
        base, _ = analyze_asset(
            self.wav,
            block_samples=9600,
            asset_id="batch",
            scheduler_config=self._config(2, 2),
        )
        combinations = [
            GateParameters(-70.0, -10.0, -1.0),
            GateParameters(-50.0, -10.0, -1.0),
            GateParameters(-70.0, -3.0, -20.0),
        ]
        first = run_batch(base["blocks"], combinations, asset_id="batch")
        second = run_batch(base["blocks"], combinations, asset_id="batch")
        self.assertEqual(canonical_bytes(first), canonical_bytes(second))
        self.assertEqual(first["combination_count"], 3)
        self.assertEqual(len(first["block_ids"]), len(base["blocks"]))
        self.assertTrue(first["flipped_block_ids"])
        for block_id in first["block_ids"]:
            self.assertEqual(set(first["status_matrix"][block_id]), {"0000", "0001", "0002"})


if __name__ == "__main__":
    unittest.main()

