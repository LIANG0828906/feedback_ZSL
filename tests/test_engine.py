"""End-to-end verification of every business objective on a small sample."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest

from window_engine.batch import run_batch
from window_engine.config import FaultPlan, JobConfig
from window_engine.datagen import GenSpec, generate
from window_engine.engine import Engine, KillAfterCommit
from window_engine.errors import InputError, StateError
from window_engine.io_sources import key_counts_duckdb
from window_engine.report import business_fingerprint
from window_engine.semantics import SplitPlan, Watermark, window_start
from window_engine.util import read_json, stable_hash


class EngineCase(unittest.TestCase):
    def setUp(self) -> None:
        self.root = tempfile.mkdtemp(prefix="we-test-")
        self.data_dir = os.path.join(self.root, "data")
        self.state_dir = os.path.join(self.root, "state")
        self.result_dir = os.path.join(self.root, "results")
        self.spill_dir = os.path.join(self.root, "spill")
        self.spec = GenSpec(
            history_rows=3000,
            incremental_rows=1000,
            batches=10,
            history_hours=4,
        )
        self.manifest = generate(self.data_dir, self.spec)

    def tearDown(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)

    def _cfg(self, **overrides) -> JobConfig:
        params = {
            "data_dir": self.data_dir,
            "state_dir": self.state_dir,
            "result_dir": self.result_dir,
            "spill_dir": self.spill_dir,
            "allowed_lateness_ms": 120_000,
            "partitions": 4,
            "workers": 2,
            "state_memory_bytes": 128 * 1024,
        }
        params.update(overrides)
        return JobConfig(**params)

    def _split_plan(self, cfg: JobConfig) -> SplitPlan:
        counts = key_counts_duckdb(cfg.data_dir, self.manifest)
        return SplitPlan.build(counts, cfg.partitions, cfg.skew_ratio)

    def _run_mixed(self, cfg: JobConfig, fault_plan: FaultPlan | None = None):
        engine = Engine(cfg, self.manifest, self._split_plan(cfg), fault_plan)
        engine.open()
        report = engine.run_mixed()
        engine.close()
        return report

    # ------------------------------------------------ objective 1 & 5
    def test_batch_stream_identical_and_partition_invariant(self) -> None:
        cfg = self._cfg()
        batch_report = run_batch(cfg, self.manifest, self._split_plan(cfg))
        mixed_report = self._run_mixed(cfg)
        self.assertEqual(
            business_fingerprint(batch_report),
            business_fingerprint(mixed_report),
        )
        for partitions in (1, 2, 3, 6):
            other = self._cfg(
                partitions=partitions,
                state_dir=os.path.join(self.state_dir, f"p{partitions}"),
                spill_dir=os.path.join(self.spill_dir, f"p{partitions}"),
            )
            report = self._run_mixed(other)
            self.assertEqual(
                report["windows"], mixed_report["windows"], f"partitions={partitions}"
            )
            self.assertEqual(
                report["dedup_dropped"], mixed_report["dedup_dropped"]
            )

    def test_deterministic_generation(self) -> None:
        again = generate(os.path.join(self.root, "data2"), self.spec)
        self.assertEqual(
            again["history_row_count"], self.manifest["history_row_count"]
        )
        self.assertEqual(again["duplicate_rows"], self.manifest["duplicate_rows"])

    # ------------------------------------------------ objective 2
    def test_watermark_lateness_semantics(self) -> None:
        strict = self._cfg(allowed_lateness_ms=0)
        relaxed = self._cfg(allowed_lateness_ms=600_000)
        strict_report = self._run_mixed(strict)
        relaxed_report = self._run_mixed(relaxed)
        accepted = lambda r: sum(
            w["event_count"] for w in r["windows"]
        )
        # Zero tolerance closes windows immediately, so fewer out-of-order
        # events are accepted; a large tolerance accepts every event whose
        # delay stays inside the bound.
        self.assertLessEqual(
            accepted(strict_report), accepted(relaxed_report)
        )
        self.assertEqual(
            accepted(strict_report) + strict_report["late_events"],
            accepted(relaxed_report) + relaxed_report["late_events"],
        )

    def test_watermark_is_monotonic(self) -> None:
        wm = Watermark(1000)
        values = [wm.observe(t) for t in (5000, 4000, 9000, 8000)]
        self.assertEqual(values, sorted(values))
        self.assertEqual(values[0], 4000)
        self.assertEqual(wm.finalize(), 10**15)

    # ------------------------------------------------ objective 3 & 4
    def test_kill_resume_no_duplicate_output(self) -> None:
        cfg = self._cfg()
        fault = FaultPlan.load(None)
        from window_engine.config import FaultRule

        fault.rules = [FaultRule(fault="kill_after_commit", batch=4)]
        engine = Engine(cfg, self.manifest, self._split_plan(cfg), fault)
        engine.open()
        with self.assertRaises(KillAfterCommit):
            engine.run_mixed()
        engine.close()

        resumed = Engine(cfg, self.manifest, self._split_plan(cfg))
        was_resumed = resumed.open()
        self.assertTrue(was_resumed)
        report = resumed.run_mixed()
        resumed.close()

        uninterrupted = self._run_mixed(self._cfg(
            state_dir=os.path.join(self.root, "state-fresh"),
            spill_dir=os.path.join(self.root, "spill-fresh"),
        ))
        self.assertEqual(report["windows"], uninterrupted["windows"])
        self.assertEqual(
            report["dedup_dropped"], uninterrupted["dedup_dropped"]
        )
        self.assertEqual(report["recovery"]["recoveries"], 1)

        # Repeating the finished commit point emits nothing new.
        again = Engine(cfg, self.manifest, self._split_plan(cfg))
        again.open()
        repeat = again.build_report()
        again.close()
        self.assertEqual(repeat["windows"], report["windows"])
        self.assertEqual(repeat["dedup_dropped"], report["dedup_dropped"])

    def test_state_locked_returns_e_state(self) -> None:
        cfg = self._cfg()
        from window_engine.config import FaultRule

        fault = FaultPlan()
        fault.rules = [FaultRule(fault="state_locked", batch=3)]
        engine = Engine(cfg, self.manifest, self._split_plan(cfg), fault)
        engine.open()
        with self.assertRaises(StateError) as ctx:
            engine.run_mixed()
        self.assertEqual(ctx.exception.code, "E_STATE")
        engine.close()

    def test_state_corrupt_discards_checkpoint(self) -> None:
        cfg = self._cfg()
        from window_engine.config import FaultRule

        fault = FaultPlan()
        fault.rules = [FaultRule(fault="state_corrupt", batch=3)]
        engine = Engine(cfg, self.manifest, self._split_plan(cfg), fault)
        engine.open()
        with self.assertRaises(StateError) as ctx:
            engine.run_mixed()
        self.assertEqual(ctx.exception.code, "E_STATE")
        engine.close()
        remaining = set(os.listdir(os.path.join(cfg.state_dir, "job")))
        self.assertEqual(remaining, {"LOCK"})

    def test_batch_read_failure_returns_e_input(self) -> None:
        cfg = self._cfg()
        from window_engine.config import FaultRule

        fault = FaultPlan()
        fault.rules = [FaultRule(fault="batch_read_failure", batch=5)]
        engine = Engine(cfg, self.manifest, self._split_plan(cfg), fault)
        engine.open()
        with self.assertRaises(InputError) as ctx:
            engine.run_mixed()
        self.assertEqual(ctx.exception.code, "E_INPUT")
        engine.close()

    def test_early_watermark_records_fault_and_raises_late_count(self) -> None:
        normal = self._run_mixed(self._cfg(
            state_dir=os.path.join(self.root, "state-n"),
            spill_dir=os.path.join(self.root, "spill-n"),
        ))
        cfg = self._cfg()
        from window_engine.config import FaultRule

        fault = FaultPlan()
        fault.rules = [FaultRule(fault="early_watermark", batch=2)]
        report = self._run_mixed(cfg, fault)
        self.assertEqual(report["faults"][0]["fault"], "early_watermark")
        self.assertGreaterEqual(
            report["late_events"], normal["late_events"]
        )

    # ------------------------------------------------ objective 5
    def test_split_plan_respects_skew_ratio(self) -> None:
        cfg = self._cfg(partitions=4)
        counts = key_counts_duckdb(cfg.data_dir, self.manifest)
        plan = SplitPlan.build(counts, 4, 0.40)
        total = sum(counts.values())
        loads = [0] * 4
        for key, count in counts.items():
            if key in plan.split_subunits:
                base, remainder = divmod(count, 4)
                for unit in range(4):
                    loads[unit] += base + (1 if unit < remainder else 0)
            else:
                loads[stable_hash(key) % 4] += count
        self.assertLessEqual(max(loads) / total, 0.40 + 1e-9)

    # ------------------------------------------------ objective 7
    def test_report_fields_sorted_and_stable(self) -> None:
        report = self._run_mixed(self._cfg())
        required = {
            "input",
            "dedup_dropped",
            "late_events",
            "window_count",
            "windows",
            "split_plan",
            "recovery",
            "spill_bytes",
            "peak_rss_bytes",
            "faults",
        }
        self.assertTrue(required.issubset(report))
        again = self._run_mixed(self._cfg(
            state_dir=os.path.join(self.root, "state2"),
            spill_dir=os.path.join(self.root, "spill2"),
        ))
        self.assertEqual(
            business_fingerprint(report), business_fingerprint(again)
        )

    def test_window_assignment(self) -> None:
        self.assertEqual(window_start(123_456, 60_000), 120_000)

    def test_invalid_config_returns_e_config(self) -> None:
        from window_engine.config import JobConfig
        from window_engine.errors import ConfigError

        bad = os.path.join(self.root, "bad.json")
        with open(bad, "w", encoding="utf-8") as fh:
            json.dump({"allowed_lateness_ms": -1}, fh)
        with self.assertRaises(ConfigError) as ctx:
            JobConfig.load(bad)
        self.assertEqual(ctx.exception.code, "E_CONFIG")


if __name__ == "__main__":
    unittest.main()
