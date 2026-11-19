from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from settlement_rollout.config import load_configuration
from settlement_rollout.errors import ConfigError

from .helpers import ROOT, quick_plan_copy, quick_slo_copy


class ConfigValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.slo = quick_slo_copy(self.directory)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _plan(self, mutate=None) -> Path:
        plan = quick_plan_copy(self.directory)
        value = json.loads(plan.read_text(encoding="utf-8"))
        if mutate is not None:
            mutate(value)
        plan.write_text(
            json.dumps(value, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        return plan

    def _assert_code(self, plan: Path, code: str) -> None:
        with self.assertRaises(ConfigError) as caught:
            load_configuration(plan, self.slo)
        self.assertEqual(caught.exception.code, code)

    def test_unknown_instance(self) -> None:
        def mutate(plan):
            plan["batches"][0]["instances"].append("ghost-instance")

        self._assert_code(self._plan(mutate), "CFG_UNKNOWN_INSTANCE")

    def test_empty_batch(self) -> None:
        def mutate(plan):
            plan["batches"][0]["instances"] = []

        self._assert_code(self._plan(mutate), "CFG_EMPTY_BATCH")

    def test_availability_target_range(self) -> None:
        slo = json.loads(self.slo.read_text(encoding="utf-8"))
        slo["availability"]["target_percent"] = 100.1
        self.slo.write_text(json.dumps(slo), encoding="utf-8")
        self._assert_code(self._plan(), "CFG_TARGET_RANGE")

    def test_latency_target_range(self) -> None:
        slo = json.loads(self.slo.read_text(encoding="utf-8"))
        slo["latency"]["percentile"] = -0.1
        self.slo.write_text(json.dumps(slo), encoding="utf-8")
        self._assert_code(self._plan(), "CFG_TARGET_RANGE")

    def test_non_positive_window(self) -> None:
        self._assert_code(
            self._plan(
                lambda plan: plan.__setitem__(
                    "observation_window_seconds", 0
                )
            ),
            "CFG_WINDOW_INVALID",
        )

    def test_cli_refuses_before_creating_state(self) -> None:
        plan = self._plan(
            lambda value: value["batches"][0].__setitem__(
                "instances", ["missing"]
            )
        )
        result = subprocess.run(
            [
                sys.executable,
                "-m",
                "settlement_rollout",
                "validate",
                "--plan",
                str(plan),
                "--slo",
                str(self.slo),
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 2)
        payload = json.loads(result.stderr)
        self.assertEqual(payload["code"], "CFG_UNKNOWN_INSTANCE")
        self.assertFalse((self.directory / "state.sqlite3").exists())


if __name__ == "__main__":
    unittest.main()
