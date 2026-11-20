from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


class CliTests(unittest.TestCase):
    def test_shortcut_flags_and_rounding_override(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "report.json"
            completed = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "docagg",
                    "run",
                    "--input",
                    "samples/anomalies.jsonl",
                    "--group-by",
                    "tenant",
                    "--sum",
                    "amount",
                    "--avg",
                    "amount",
                    "--stddev",
                    "amount",
                    "--rounding",
                    "sum=half_even@8",
                    "--max-active-groups",
                    "1",
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            report = json.loads(output.read_text(encoding="utf-8"))
            self.assertEqual(
                report["pipeline"]["rounding"]["sum"],
                {"mode": "half_even", "significant_digits": 8},
            )
            self.assertTrue(report["spill"]["triggered"])

    def test_precision_failure_goes_to_stderr_and_exit_two(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            reference_path = Path(directory) / "reference.json"
            reference = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "docagg",
                    "reference",
                    "--input",
                    "samples/events.jsonl",
                    "--pipeline",
                    "samples/pipeline.json",
                    "--output",
                    str(reference_path),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(reference.returncode, 0, reference.stderr)
            payload = json.loads(reference_path.read_text(encoding="utf-8"))
            for group in payload["groups"]:
                aggregate = group["aggregates"].get("amount_sum")
                if aggregate and aggregate.get("status") == "ok":
                    aggregate["value_text"] = str(
                        float(aggregate["value_text"]) + 1_000_000
                    )
                    break
            reference_path.write_text(
                json.dumps(payload, ensure_ascii=False, sort_keys=True),
                encoding="utf-8",
            )
            output = Path(directory) / "report.json"
            completed = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "docagg",
                    "run",
                    "--input",
                    "samples/events.jsonl",
                    "--pipeline",
                    "samples/pipeline.json",
                    "--reference-file",
                    str(reference_path),
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 2)
            self.assertIn("E_PRECISION_BOUND", completed.stderr)
            report = json.loads(output.read_text(encoding="utf-8"))
            self.assertGreater(report["summary"]["precision_error_count"], 0)


if __name__ == "__main__":
    unittest.main()
