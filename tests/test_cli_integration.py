from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

from npr_workbench.cli import main

from tests.helpers import ROOT


class CliIntegrationTests(unittest.TestCase):
    def test_three_path_verification_cli_passes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "verification"
            stdout = io.StringIO()
            with contextlib.redirect_stdout(stdout):
                code = main(
                    [
                        "verify",
                        "--data",
                        str(ROOT / "data"),
                        "--scene",
                        "orb_and_crate",
                        "--pause-after",
                        "2",
                        "--out",
                        str(output),
                    ]
                )
            self.assertEqual(code, 0, stdout.getvalue()[-2000:])
            payload = json.loads((output / "verification.json").read_text(encoding="utf-8"))
            self.assertTrue(payload["passed"])
            self.assertTrue(payload["path_report"]["pixel_exact"])
            self.assertEqual(
                len(set(payload["path_report"]["styles_sha256"].values())),
                1,
            )


if __name__ == "__main__":
    unittest.main()

