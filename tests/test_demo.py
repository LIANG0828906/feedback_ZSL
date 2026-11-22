from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from sync_kernel.demo_runner import run_demo


class DemoTests(unittest.TestCase):
    def test_single_command_demo_report_passes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            report = run_demo(workspace=root, ticks=60, generate=True)
            self.assertEqual(report["summary"]["overall"], "PASS")
            self.assertEqual(report["summary"]["failed"], 0)
            self.assertIsNone(report["convergence"]["first_divergence"])
            self.assertFalse(
                report["forced_termination_boundary"][
                    "original_process_report_complete"
                ]
            )
            json_path = root / "artifacts" / "demo" / "report.json"
            markdown_path = root / "artifacts" / "demo" / "report.md"
            self.assertTrue(json_path.exists())
            self.assertTrue(markdown_path.exists())
            on_disk = json.loads(json_path.read_text(encoding="utf-8"))
            self.assertEqual(on_disk["summary"]["passed"], 10)


if __name__ == "__main__":
    unittest.main()

