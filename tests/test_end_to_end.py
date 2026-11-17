from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from drum_bus.manifest import load_project
from drum_bus.render import render_project
from drum_bus.synth import DemoConfig, generate_demo_project
from drum_bus.wavio import read_wav


CASE_CONFIG = DemoConfig(
    duration_seconds=2.4,
    reference_times=(0.25, 0.58, 0.91, 1.27, 1.68, 2.02),
    block_size=1024,
    target_lufs=-16.0,
    true_peak_db=-1.0,
)


def _hash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class EndToEndTests(unittest.TestCase):
    def test_acceptance_metrics_and_determinism(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = generate_demo_project(root / "case", CASE_CONFIG)
            project = load_project(manifest)
            first = render_project(project, root / "first")
            second = render_project(project, root / "second")
            self.assertEqual(first.status, "complete")
            self.assertEqual(second.status, "complete")
            self.assertEqual(_hash(first.bus_path), _hash(second.bus_path))

            complete = read_wav(first.bus_path)
            expected_frames = max(wav.info.frames for wav in project.wav_data.values())
            self.assertEqual(complete.info.frames, expected_frames)
            self.assertEqual(complete.info.channels, 2)

            report = json.loads((root / "first" / "render_report.json").read_text())
            self.assertEqual(report["warnings"], [])
            by_name = {channel["channel"]: channel for channel in report["channels"]}
            offsets = {
                "board_drum": CASE_CONFIG.board_offset,
                "bangzi": CASE_CONFIG.bangzi_offset,
                "tanggu": CASE_CONFIG.tanggu_offset,
            }
            for name, offset in offsets.items():
                channel = by_name[name]
                self.assertEqual(channel["status"], "OK")
                self.assertEqual(channel["alignment_shift_samples"], -offset)
                self.assertLessEqual(channel["max_alignment_error_samples"], 1)
            for name in ("board_drum", "bangzi"):
                reflection = by_name[name]["reflection"]
                self.assertGreaterEqual(reflection["window_energy_reduction_db"], 12.0)
                self.assertLessEqual(reflection["transient_peak_loss_db"], 1.0)
                self.assertTrue(reflection["outside_window_unchanged"])
                self.assertEqual(reflection["estimated_delays_samples"], [456, 864])
                for measured, declared in zip(
                    reflection["estimated_gains_db"], [-11.5, -16.5]
                ):
                    self.assertLessEqual(abs(measured - declared), 0.8)

            self.assertLessEqual(abs(report["master"]["deviation_lu"]), 0.5)
            self.assertLessEqual(report["master"]["true_peak_dbtp"], -1.0 + 0.002)
            self.assertGreaterEqual(
                report["master"]["limiter_latency_compensation_samples"], 0
            )
            ratio_sum = sum(
                channel["contribution_ratio"] for channel in report["channels"]
            )
            self.assertAlmostEqual(ratio_sum, 1.0, delta=1e-9)

            first_hash = _hash(first.bus_path)
            render_project(project, root / "first")
            self.assertEqual(_hash(first.bus_path), first_hash)
            snapshot = json.loads(
                (root / "first" / ".render_state" / "snapshot.json").read_text()
            )
            self.assertEqual(snapshot["status"], "complete")
            self.assertEqual(snapshot["completed_blocks"], snapshot["total_blocks"])


if __name__ == "__main__":
    unittest.main()

