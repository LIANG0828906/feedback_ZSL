from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from acoustic_workbench.diagnostics import run_diagnostics
from acoustic_workbench.samplegen import VENUES, generate_venue


class DiagnosticsTests(unittest.TestCase):
    def test_sample_generator_is_byte_deterministic(self) -> None:
        with tempfile.TemporaryDirectory(prefix="acoustic-sample-tests-") as raw:
            root = Path(raw)
            generate_venue(
                root / "first",
                "club",
                VENUES["club"],
                build_baseline=False,
            )
            generate_venue(
                root / "second",
                "club",
                VENUES["club"],
                build_baseline=False,
            )
            first = root / "first" / "venue_club"
            second = root / "second" / "venue_club"
            for relative in (
                "scene.json",
                "materials.json",
                "ir_layout.json",
                "ir_L1.wav",
                "ir_L2.wav",
                "test_signal.wav",
            ):
                self.assertEqual(
                    (first / relative).read_bytes(),
                    (second / relative).read_bytes(),
                    relative,
                )

    def test_trace_and_fields_are_byte_deterministic(self) -> None:
        with tempfile.TemporaryDirectory(prefix="acoustic-diag-tests-") as raw:
            root = Path(raw)
            sample_root = root / "samples"
            generate_venue(
                sample_root,
                "club",
                VENUES["club"],
                build_baseline=False,
            )
            venue_dir = sample_root / "venue_club"
            changes = root / "changes.json"
            changes.write_text(
                """
[
  {"factor":"material","wall_id":"east","material":"acoustic_panel"},
  {"factor":"source_position","position":[2.8,4.6,1.4]},
  {"factor":"listener_position","listener_id":"L1","position":[8.2,5.5,1.2]},
  {"factor":"band_config","center_frequencies":[125,250,500,1000,2000]},
  {"factor":"window","window":{"start_ms":1.5,"end_ms":66.0}}
]
""".strip(),
                encoding="utf-8",
            )
            trace_one = root / "one" / "trace.jsonl"
            trace_two = root / "two" / "trace.jsonl"
            first = run_diagnostics(
                venue_dir,
                changes,
                trace_one,
                output_dir=root / "one",
            )
            second = run_diagnostics(
                venue_dir,
                changes,
                trace_two,
                output_dir=root / "two",
            )
            self.assertTrue(first["all_within_tolerance"])
            self.assertTrue(second["all_within_tolerance"])
            self.assertEqual(trace_one.read_bytes(), trace_two.read_bytes())
            self.assertEqual(first["comparisons"], second["comparisons"])


if __name__ == "__main__":
    unittest.main()

