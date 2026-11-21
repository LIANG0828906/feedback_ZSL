from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from a11y_rhythm.compliance import build_compliance_report
from a11y_rhythm.samples import Scenario, generate_scenario
from a11y_rhythm.store import DeliverySession

from tests.helpers import conflict_fixture, item, load_fixture, write_fixture


class SampleAndComplianceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_sample_generator_is_byte_deterministic(self) -> None:
        scenario = Scenario(
            "tiny",
            seed=12345,
            speech_count=4,
            description_count=2,
            duration_seconds=6,
            channels=1,
            max_cps=14.0,
            contention=0.7,
        )
        first = generate_scenario(self.root / "first", scenario)
        second = generate_scenario(self.root / "second", scenario)
        self.assertEqual(first["source_hash"], second["source_hash"])
        self.assertEqual(first["files"], second["files"])
        for name in first["files"]:
            self.assertEqual(
                (self.root / "first" / name).read_bytes(),
                (self.root / "second" / name).read_bytes(),
            )

    def test_compliance_report_is_self_contained_per_statement(self) -> None:
        fixture = write_fixture(
            self.root,
            name="unreadable-report",
            duration_seconds=4.0,
            speech=[item("sp001", 0.2, 2.0, "A spoken sentence with enough room.", priority=80)],
            descriptions=[
                item(
                    "ds001",
                    0.5,
                    0.9,
                    "This description is intentionally much longer than its available reading window allows.",
                    speaker="AUDIO_DESCRIPTION",
                    priority=20,
                    max_cps=12.0,
                    min_gap_ms=150,
                )
            ],
        )
        bundle, frames, coverage, _ = load_fixture(fixture)
        session = DeliverySession.open(
            bundle, frames, coverage, self.root / "report.session.json"
        )
        snapshot = session.initialize()
        report = snapshot.payload["compliance"]
        self.assertFalse(report["compliant"])
        self.assertTrue(report["violations"])
        for violation in report["violations"]:
            self.assertIn("type", violation)
            self.assertIn("description_id", violation)
            self.assertIn("description_interval", violation)
            self.assertIn("conflict_objects", violation)
            self.assertIn("minimum_duration_samples", violation)
            self.assertIn("actual_duration_samples", violation)
        self.assertTrue(
            any(
                check["check"] in {"status", "readability", "overflow", "min_gap"}
                for desc in report["descriptions"]
                for check in desc["checks"]
            )
        )

    def test_trace_logs_are_byte_identical_for_same_operations(self) -> None:
        fixture = conflict_fixture(self.root)
        bundle, frames, coverage, _ = load_fixture(fixture)
        traces = []
        for index in range(2):
            session = DeliverySession.open(
                bundle,
                frames,
                coverage,
                self.root / f"deterministic-{index}.session.json",
            )
            session.initialize()
            session.update_item(
                "ds001",
                {"text": "A longer deterministic description for the same operation sequence."},
            )
            traces.append(session.store.trace_path.read_bytes())
        self.assertEqual(traces[0], traces[1])


if __name__ == "__main__":
    unittest.main()
