from __future__ import annotations

import contextlib
import io
import tempfile
import unittest
from pathlib import Path

from motionreview.cli import main


class CLITest(unittest.TestCase):
    def test_generate_solve_and_verify_small_dataset(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            samples = root / "samples"
            with contextlib.redirect_stdout(io.StringIO()):
                generate_rc = main(
                    [
                        "generate",
                        "--out",
                        str(samples),
                        "--case",
                        "tiny:12:4",
                        "--max-iterations",
                        "30",
                    ]
                )
            self.assertEqual(generate_rc, 0)
            config_path = samples / "run_config.json"
            output = root / "solve"
            with contextlib.redirect_stdout(io.StringIO()):
                solve_rc = main(
                    [
                        "solve",
                        "--sample",
                        str(samples),
                        "--case-name",
                        "tiny",
                        "--config",
                        str(config_path),
                        "--out",
                        str(output),
                    ]
                )
            self.assertEqual(solve_rc, 0)
            with contextlib.redirect_stdout(io.StringIO()) as stdout:
                verify_rc = main(
                    [
                        "verify",
                        "--sample",
                        str(samples),
                        "--config",
                        str(config_path),
                    ]
                )
            self.assertEqual(verify_rc, 0)
            self.assertIn('"ok": true', stdout.getvalue())


if __name__ == "__main__":
    unittest.main()
