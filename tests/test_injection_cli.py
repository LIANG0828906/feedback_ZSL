from __future__ import annotations

import contextlib
import copy
import io
import json
import tempfile
import unittest
from pathlib import Path

from levelworkbench.cli import main
from levelworkbench.config import SUPPORTED_INJECTIONS, load_config
from levelworkbench.errors import (
    E_INJECT_CONSTRAINT_LIMIT,
    E_INJECT_DANGLING_DOOR,
    E_INJECT_KEY_CYCLE,
    E_INJECT_ROOM_BOUNDS,
    E_INJECT_TRUNCATED_INPUT,
    WorkbenchError,
)
from levelworkbench.pipeline import generate_from_config


ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


EXPECTED_CODES = {
    "room_bounds": E_INJECT_ROOM_BOUNDS,
    "dangling_door": E_INJECT_DANGLING_DOOR,
    "key_cycle": E_INJECT_KEY_CYCLE,
    "constraint_limit": E_INJECT_CONSTRAINT_LIMIT,
    "truncated_input": E_INJECT_TRUNCATED_INPUT,
}


class InjectionTests(unittest.TestCase):
    def test_each_injection_has_distinct_code_and_location(self) -> None:
        self.assertEqual(set(SUPPORTED_INJECTIONS), set(EXPECTED_CODES))
        observed = set()
        for mode, expected in EXPECTED_CODES.items():
            with self.assertRaises(WorkbenchError) as caught:
                load_config(SAMPLES / "injection_base.json", injection=mode)
            error = caught.exception
            observed.add(error.code)
            self.assertEqual(error.code, expected, mode)
            self.assertIsNotNone(error.injection_point, mode)
            self.assertIn("location", error.injection_point, mode)
        self.assertEqual(len(observed), 5)

    def test_cli_injection_never_creates_output_files(self) -> None:
        for mode in sorted(EXPECTED_CODES):
            with tempfile.TemporaryDirectory() as temporary:
                out_dir = Path(temporary) / "should-not-exist"
                stdout = io.StringIO()
                with contextlib.redirect_stdout(stdout):
                    rc = main(
                        [
                            "--config",
                            str(SAMPLES / "injection_base.json"),
                            "--inject",
                            mode,
                            "--out",
                            str(out_dir),
                        ]
                    )
                self.assertEqual(rc, 6, mode)
                self.assertFalse(out_dir.exists(), mode)
                payload = json.loads(stdout.getvalue())
                self.assertEqual(payload["code"], EXPECTED_CODES[mode])

    def test_invalid_trace_cli_writes_no_level(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            config = load_config(SAMPLES / "small.json")
            result = generate_from_config(config)
            broken = copy.deepcopy(result.trace)
            broken["events"] = broken["events"][:-2]
            trace_path = temporary_path / "broken_trace.json"
            trace_path.write_text(json.dumps(broken), encoding="utf-8")
            out_dir = temporary_path / "out"
            stdout = io.StringIO()
            with contextlib.redirect_stdout(stdout):
                rc = main(["--replay", str(trace_path), "--out", str(out_dir)])
            self.assertEqual(rc, 4)
            self.assertFalse(out_dir.exists())
            payload = json.loads(stdout.getvalue())
            self.assertEqual(payload["code"], "E_TRACE_INVALID")


if __name__ == "__main__":
    unittest.main()
