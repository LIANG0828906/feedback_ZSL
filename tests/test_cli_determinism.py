from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[1]


class CliDeterminismTests(unittest.TestCase):
    def _run(self, arguments: list[str]) -> subprocess.CompletedProcess[bytes]:
        return subprocess.run(
            [sys.executable, "-m", "loudness_diag", *arguments],
            cwd=PROJECT_ROOT,
            capture_output=True,
            check=False,
        )

    def test_diagnose_and_batch_are_byte_identical_across_runs(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            generated = self._run(
                [
                    "generate",
                    "--output",
                    str(root / "samples"),
                    "--seconds",
                    "1.2",
                ]
            )
            self.assertEqual(generated.returncode, 0, generated.stderr.decode())
            config = root / "config.json"
            config.write_text(
                json.dumps(
                    {
                        "parallelism": 3,
                        "memory_budget_bytes": 1843200,
                        "block_memory_bytes": 921600,
                    }
                ),
                encoding="utf-8",
            )
            params = root / "params.json"
            params.write_text(
                json.dumps(
                    {
                        "combinations": [
                            {
                                "absolute_lufs": -70.0,
                                "relative_offset_lu": -10.0,
                                "true_peak_limit_dbtp": -1.0,
                            },
                            {
                                "absolute_lufs": -50.0,
                                "relative_offset_lu": -4.0,
                                "true_peak_limit_dbtp": -12.0,
                            },
                        ]
                    }
                ),
                encoding="utf-8",
            )
            common = [
                "--wav",
                str(root / "samples" / "program_51_24bit.wav"),
                "--asset-id",
                "deterministic",
                "--block-samples",
                "9600",
                "--config",
                str(config),
            ]
            for name in ("first", "second"):
                result = self._run(
                    ["diagnose", "--output", str(root / name), *common]
                )
                self.assertEqual(result.returncode, 0, result.stderr.decode())
            self.assertEqual(
                (root / "first" / "report.json").read_bytes(),
                (root / "second" / "report.json").read_bytes(),
            )
            self.assertEqual(
                (root / "first" / "trace.log").read_bytes(),
                (root / "second" / "trace.log").read_bytes(),
            )

            for name in ("batch-first", "batch-second"):
                result = self._run(
                    [
                        "batch",
                        "--output",
                        str(root / name),
                        "--params",
                        str(params),
                        *common,
                    ]
                )
                self.assertEqual(result.returncode, 0, result.stderr.decode())
            self.assertEqual(
                (root / "batch-first" / "batch_report.json").read_bytes(),
                (root / "batch-second" / "batch_report.json").read_bytes(),
            )
            self.assertEqual(
                (root / "batch-first" / "trace.log").read_bytes(),
                (root / "batch-second" / "trace.log").read_bytes(),
            )

    def test_header_error_is_returned_as_business_code(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            bad = Path(directory) / "bad.wav"
            bad.write_bytes(b"not a wav")
            result = self._run(
                [
                    "diagnose",
                    "--wav",
                    str(bad),
                    "--output",
                    str(Path(directory) / "out"),
                    "--asset-id",
                    "bad",
                ]
            )
            self.assertEqual(result.returncode, 2)
            payload = json.loads(result.stderr)
            self.assertEqual(payload["error"]["code"], "E_HEADER_INVALID")


if __name__ == "__main__":
    unittest.main()

