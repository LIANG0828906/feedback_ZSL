from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from iovflow.errors import EngineError
from iovflow.generator import GeneratorConfig, generate
from iovflow.orchestrator import run_job

from .helpers import make_config


ROOT = Path(__file__).resolve().parents[1]


class ResourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._tmp.name)
        generate(
            GeneratorConfig(
                out_dir=str(cls.root / "data"),
                rows=3000,
                batches=10,
                vehicles=40,
            )
        )
        (cls.root / "window_5m.sql").write_text(
            (ROOT / "sql/window_5m.sql").read_text(encoding="utf-8"), encoding="utf-8"
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls._tmp.cleanup()

    def test_state_spill_is_reported(self) -> None:
        report = run_job(
            make_config(
                self.root,
                "spill",
                "batch",
                state_memory_mb=0.001,
            )
        )
        self.assertGreater(report["resources"]["spill_bytes"], 0)

    def test_memory_limit_fails_with_stable_code(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            run_job(
                make_config(
                    self.root,
                    "memory",
                    "stream",
                    memory_limit_mb=1,
                )
            )
        self.assertEqual(ctx.exception.code, "IOV_RESOURCE_MEMORY")
        conn = __import__("sqlite3").connect(self.root / "state-memory.db")
        try:
            row = conn.execute(
                "SELECT status, error_code FROM jobs WHERE job_id='memory'"
            ).fetchone()
        finally:
            conn.close()
        self.assertEqual(row[0], "failed")
        self.assertEqual(row[1], "IOV_RESOURCE_MEMORY")

    def test_temp_limit_fails_with_stable_code(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            run_job(
                make_config(
                    self.root,
                    "temp",
                    "batch",
                    temp_limit_mb=0,
                )
            )
        self.assertEqual(ctx.exception.code, "IOV_RESOURCE_TEMP")


if __name__ == "__main__":
    unittest.main()

