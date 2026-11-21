from __future__ import annotations

import copy
import json
import os
import shutil
import tempfile
import unittest
from decimal import Decimal
from types import SimpleNamespace

from replay.baseline import compare_baseline
from replay.compare import compare_paths
from replay.engine import ReplayEngine, run_recording
from replay.error_model import ErrorTracker
from replay.errors import ReplayError
from replay.generator import generate_samples
from replay.lock import MatchLock
from replay.model import (
    STATE_MODEL_VERSION,
    canonical_state_json,
    decimal_string,
)
from replay.snapshot import SnapshotStore
from replay.trajectory import trajectory_bytes
from replay.validation import (
    atomic_write_json,
    load_recording,
    parse_recording,
)
from replay.versioning import downgrade_run, downgrade_state


def _minimal_raw(
    *,
    total_steps: int = 6,
    source_version: str = "v2",
    ops: list[dict] | None = None,
) -> dict:
    if ops is None:
        ops = [
            {
                "id": "op-1",
                "entity_id": "p1",
                "client_seq": 1,
                "arrival_seq": 1,
                "arrival_step": 1,
                "effective_step": 1,
                "speed": "0.25",
                "heading": "0",
                "resource_delta": "0.125",
            },
            {
                "id": "op-2",
                "entity_id": "p1",
                "client_seq": 2,
                "arrival_seq": 2,
                "arrival_step": 2,
                "effective_step": 2,
                "speed": "0.5",
                "heading": "0.1",
                "resource_delta": "0.25",
            },
        ]
    entity = {
        "id": "p1",
        "position": {"x": "0.0625", "y": "-0.125"},
        "velocity": {"x": "0", "y": "0"},
        "heading": "0",
    }
    if source_version == "v2":
        entity["alive"] = True
    return {
        "recording_version": source_version,
        "match_id": "match-minimal",
        "config": {
            "total_steps": total_steps,
            "timestep": "0.1",
            "flip_threshold": "100",
            "resource_diff_threshold": "0.01",
            "coord_limit": "100",
            "fixed_param_decimals": 3,
            "fixed_accum_decimals": 6,
            "late_window_steps": 1,
            "alive_default": True,
            "tolerances": {
                "position": "0.000001",
                "velocity": "0.000001",
                "heading": "0.000001",
                "resource": "0.000001",
            },
        },
        "initial_state": {"room": {"resource": "0"}, "entities": [entity]},
        "operations": ops,
    }


class SampleFixture(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.samples_root = tempfile.mkdtemp(prefix="replay-tests-")
        cls.manifest = generate_samples(cls.samples_root, seed=1234)
        cls.case_by_name = {case["name"]: case for case in cls.manifest["cases"]}

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.samples_root, ignore_errors=True)

    def sample(self, name: str) -> tuple[dict, str, str, str]:
        case = self.case_by_name[name]
        recording_path = os.path.join(self.samples_root, case["recording"])
        baseline_path = os.path.join(self.samples_root, case["baseline"])
        mismatch_path = os.path.join(self.samples_root, case["baseline_mismatch"])
        return case, recording_path, baseline_path, mismatch_path


class ValidationTests(unittest.TestCase):
    def test_invalid_step_path(self) -> None:
        raw = _minimal_raw()
        raw["operations"][0]["effective_step"] = 99
        with self.assertRaises(ReplayError) as caught:
            parse_recording(raw)
        self.assertEqual(caught.exception.code, "E_INPUT_INVALID")
        self.assertEqual(caught.exception.path, "operations[0].effective_step")

    def test_missing_parameter_path(self) -> None:
        raw = _minimal_raw()
        raw["operations"][0].pop("speed")
        with self.assertRaises(ReplayError) as caught:
            parse_recording(raw)
        self.assertEqual(caught.exception.code, "E_INPUT_INVALID")
        self.assertEqual(caught.exception.path, "operations[0].speed")

    def test_nonfinite_heading(self) -> None:
        raw = _minimal_raw()
        raw["operations"][0]["heading"] = float("nan")
        with self.assertRaises(ReplayError) as caught:
            parse_recording(raw)
        self.assertEqual(caught.exception.code, "E_NONFINITE")
        self.assertEqual(caught.exception.path, "operations[0].heading")

    def test_duplicate_client_sequence(self) -> None:
        raw = _minimal_raw()
        raw["operations"][1]["client_seq"] = raw["operations"][0]["client_seq"]
        with self.assertRaises(ReplayError) as caught:
            parse_recording(raw)
        self.assertEqual(caught.exception.code, "E_DUP_CLIENT_SEQ")
        self.assertEqual(caught.exception.path, "operations[1].client_seq")

    def test_out_of_bounds_initial_coordinate(self) -> None:
        raw = _minimal_raw()
        raw["initial_state"]["entities"][0]["position"]["x"] = "101"
        with self.assertRaises(ReplayError) as caught:
            parse_recording(raw)
        self.assertEqual(caught.exception.code, "E_COORD_RANGE")
        self.assertEqual(caught.exception.path, "initial_state.entities[0].position")

    def test_late_operation(self) -> None:
        raw = _minimal_raw()
        raw["operations"][0]["arrival_step"] = 3
        with self.assertRaises(ReplayError) as caught:
            parse_recording(raw)
        self.assertEqual(caught.exception.code, "E_LATE_OP")
        self.assertEqual(caught.exception.path, "operations[0].arrival_step")

    def test_stable_operation_order(self) -> None:
        raw = _minimal_raw(
            ops=[
                {
                    "id": "late-arrival",
                    "entity_id": "p1",
                    "client_seq": 1,
                    "arrival_seq": 9,
                    "arrival_step": 2,
                    "effective_step": 2,
                    "speed": "0",
                    "heading": "0",
                    "resource_delta": "0",
                },
                {
                    "id": "first",
                    "entity_id": "p1",
                    "client_seq": 2,
                    "arrival_seq": 1,
                    "arrival_step": 1,
                    "effective_step": 1,
                    "speed": "0",
                    "heading": "0",
                    "resource_delta": "0",
                },
                {
                    "id": "early-arrival",
                    "entity_id": "p1",
                    "client_seq": 3,
                    "arrival_seq": 2,
                    "arrival_step": 2,
                    "effective_step": 2,
                    "speed": "0",
                    "heading": "0",
                    "resource_delta": "0",
                },
            ]
        )
        recording = parse_recording(raw)
        self.assertEqual(
            [op.op_id for op in recording.operations],
            ["first", "early-arrival", "late-arrival"],
        )

    def test_invalid_mode(self) -> None:
        recording = parse_recording(_minimal_raw())
        with self.assertRaises(ReplayError) as caught:
            ReplayEngine(recording, "quantum")
        self.assertEqual(caught.exception.code, "E_MODE_INVALID")


class NumericPathTests(SampleFixture):
    def test_generator_is_reproducible_for_same_seed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            regenerated = generate_samples(directory, seed=1234)
        self.assertEqual(regenerated, self.manifest)

    def test_fixed_and_float_reach_expected_state_shapes(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        fixed = run_recording(recording, "fixed")
        floating = run_recording(recording, "float")
        self.assertEqual(len(fixed.steps), recording.config.total_steps)
        self.assertEqual(len(floating.steps), recording.config.total_steps)
        self.assertTrue(all("alive" in step.state["entities"][0] for step in fixed.steps))
        self.assertNotEqual(fixed.final_state_hash, floating.final_state_hash)

    def test_deviation_report_and_tolerance_steps(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        report = compare_paths(recording)
        self.assertTrue(report["deterministic_fixed"])
        self.assertTrue(report["deterministic_float"])
        self.assertGreater(len(report["deviations"]), 0)
        self.assertGreater(len(report["tolerance_exceeded_steps"]), 0)
        for item in report["deviations"][:20]:
            self.assertIn("step", item)
            self.assertIn("entity", item)
            self.assertIn("field", item)
            self.assertIn("abs_diff", item)

    def test_trajectory_logs_are_byte_deterministic(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        fixed_a = run_recording(recording, "fixed")
        fixed_b = run_recording(recording, "fixed")
        float_a = run_recording(recording, "float")
        float_b = run_recording(recording, "float")
        self.assertEqual(trajectory_bytes(fixed_a), trajectory_bytes(fixed_b))
        self.assertEqual(trajectory_bytes(float_a), trajectory_bytes(float_b))
        self.assertEqual(fixed_a.trajectory_sha256(), fixed_b.trajectory_sha256())
        self.assertEqual(float_a.trajectory_sha256(), float_b.trajectory_sha256())

    def test_cache_hits_and_error_bounds_are_logged(self) -> None:
        case, recording_path, _, _ = self.sample("case-04-exact-dual")
        del case
        recording = load_recording(recording_path)
        fixed = run_recording(recording, "fixed")
        self.assertTrue(any(step.cache_hits > 0 for step in fixed.steps))
        for step in fixed.steps:
            self.assertIn("entities", step.error_summary)
            self.assertIn("room", step.error_summary)
            self.assertIn("state_hash", step.trajectory_record())
            self.assertIn("cache_hit", step.trajectory_record())

    def test_exact_dual_sample_has_identical_state_hashes(self) -> None:
        case, recording_path, _, _ = self.sample("case-04-exact-dual")
        del case
        recording = load_recording(recording_path)
        fixed = run_recording(recording, "fixed")
        floating = run_recording(recording, "float")
        self.assertEqual(
            [step.state_hash for step in fixed.steps],
            [step.state_hash for step in floating.steps],
        )
        self.assertEqual(fixed.final_state_hash, floating.final_state_hash)
        report = compare_paths(recording, fixed_run=fixed, float_run=floating)
        self.assertEqual(report["deviations"], [])

    def test_mode_switch_keeps_input_semantics(self) -> None:
        recording = parse_recording(_minimal_raw())
        before = recording.canonical_operations()
        fixed = run_recording(recording, "fixed")
        floating = run_recording(recording, "float")
        after = recording.canonical_operations()
        self.assertEqual(before, after)
        for left, right in zip(fixed.steps, floating.steps):
            self.assertEqual(
                [op["id"] for op in left.effective_ops],
                [op["id"] for op in right.effective_ops],
            )


class ErrorModelTests(unittest.TestCase):
    def test_resource_threshold_is_untrusted_and_attributed(self) -> None:
        tracker = ErrorTracker(
            param_places=3,
            accum_places=6,
            flip_threshold=Decimal("100"),
            resource_threshold=Decimal("0.0001"),
        )
        tracker.add_fixed_resource(op_id="op-1", step=1)
        entity = SimpleNamespace(x=Decimal("0"), y=Decimal("0"))
        result = tracker.evaluate_step(
            step=1,
            entities={"p1": entity},
            room_resource=Decimal("0"),
        )
        self.assertTrue(result["untrusted"])
        self.assertEqual(result["untrusted"][0]["scope"], "resource")
        self.assertEqual(
            result["untrusted"][0]["attribution"]["factor"],
            "param.resource_delta.quantization",
        )

    def test_unresolved_attribution_raises_with_candidates(self) -> None:
        tracker = ErrorTracker(
            param_places=3,
            accum_places=6,
            flip_threshold=Decimal("1"),
            resource_threshold=Decimal("1"),
        )
        tracker.bounds["p1"] = {
            "x": Decimal("2"),
            "y": Decimal("0"),
            "vx": Decimal("0"),
            "vy": Decimal("0"),
            "heading": Decimal("0"),
        }
        tracker.components["p1"] = []
        entity = SimpleNamespace(x=Decimal("0"), y=Decimal("0"))
        with self.assertRaises(ReplayError) as caught:
            tracker.evaluate_step(
                step=1,
                entities={"p1": entity},
                room_resource=Decimal("0"),
            )
        self.assertEqual(caught.exception.code, "E_ATTR_UNRESOLVED")
        self.assertIn("candidates", caught.exception.details)


class VersioningTests(SampleFixture):
    def test_v1_upgrade_marks_derived_defaults(self) -> None:
        _, recording_path, _, _ = self.sample("case-02-v1-upgrade")
        recording = load_recording(recording_path)
        self.assertEqual(recording.source_version, "v1")
        self.assertEqual(recording.state_model_version, STATE_MODEL_VERSION)
        self.assertEqual(len(recording.derived_fields), 2)
        self.assertTrue(
            all(item["source"] == "v1_default" for item in recording.derived_fields)
        )

    def test_downgrade_with_default_alive_succeeds(self) -> None:
        recording = parse_recording(_minimal_raw())
        run = run_recording(recording, "fixed")
        exported = downgrade_run(run, "v1")
        self.assertEqual(exported["state_model_version"], "state-v1")
        for frame in exported["frames"]:
            for entity in frame["state"]["entities"]:
                self.assertNotIn("alive", entity)
        state = downgrade_state(run.steps[0].state, "v1")
        self.assertEqual(state["state_model_version"], "state-v1")

    def test_downgrade_false_alive_reports_loss(self) -> None:
        recording = parse_recording(_minimal_raw())
        run = run_recording(recording, "fixed")
        run.steps[2].state["entities"][0]["alive"] = False
        with self.assertRaises(ReplayError) as caught:
            downgrade_run(run, "v1")
        self.assertEqual(caught.exception.code, "E_VERSION_LOSS")
        self.assertEqual(caught.exception.step, 3)
        self.assertIn("affected_steps", caught.exception.details)
        self.assertIn(
            "entities.p1.alive",
            caught.exception.details["affected_fields"],
        )

    def test_state_hash_field_order_is_fixed(self) -> None:
        recording = parse_recording(_minimal_raw())
        step = run_recording(recording, "fixed").steps[0]
        canonical = canonical_state_json(
            step.step,
            step.room,
            step.entities,
            numeric="fixed",
            version=STATE_MODEL_VERSION,
        )
        self.assertTrue(canonical.startswith('{"state_model_version":"state-v2","step":1,"room":'))
        reordered = dict(reversed(list(step.state.items())))
        canonical_again = canonical_state_json(
            step.step,
            step.room,
            step.entities,
            numeric="fixed",
            version=STATE_MODEL_VERSION,
        )
        self.assertEqual(canonical, canonical_again)
        self.assertEqual(list(reordered)[0], "entities")


class SnapshotTests(SampleFixture):
    def test_recovery_resumes_without_reapplying_ops(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="fixed",
                config_digest=recording.config_digest(),
            )
            full = run_recording(recording, "fixed")
            partial = run_recording(
                recording,
                "fixed",
                snapshot_store=store,
                cancel_after=4,
            )
            self.assertTrue(partial.canceled)
            self.assertEqual(partial.last_committed_step, 4)
            snapshot = store.load_latest()
            self.assertIsNotNone(snapshot)
            self.assertEqual(snapshot.step, 4)
            recovered = ReplayEngine(
                recording,
                "fixed",
                snapshot_store=store,
            ).resume(snapshot)
            self.assertEqual(recovered.steps[0].step, 5)
            self.assertEqual(recovered.final_state_hash, full.final_state_hash)
            self.assertEqual(recovered.final_chain_hash, full.final_chain_hash)
            self.assertEqual(recovered.last_committed_step, recording.config.total_steps)

    def test_float_snapshot_recovery_matches_full_run(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="float",
                config_digest=recording.config_digest(),
            )
            full = run_recording(recording, "float")
            partial = run_recording(
                recording,
                "float",
                snapshot_store=store,
                cancel_after=5,
            )
            snapshot = store.load_latest()
            self.assertIsNotNone(snapshot)
            recovered = ReplayEngine(
                recording,
                "float",
                snapshot_store=store,
            ).resume(snapshot)
            self.assertEqual(partial.last_committed_step, 5)
            self.assertEqual(recovered.steps[0].step, 6)
            self.assertEqual(recovered.final_state_hash, full.final_state_hash)

    def test_truncated_snapshot_reports_first_broken_step(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="fixed",
                config_digest=recording.config_digest(),
            )
            run_recording(recording, "fixed", snapshot_store=store, cancel_after=3)
            path = os.path.join(directory, "step-000003.json")
            with open(path, "r+b") as handle:
                data = handle.read()
                handle.seek(0)
                handle.truncate(len(data) // 2)
            with self.assertRaises(ReplayError) as caught:
                store.load_latest()
            self.assertEqual(caught.exception.code, "E_RECOVERY_INVALID")
            self.assertEqual(caught.exception.step, 3)

    def test_missing_snapshot_field_reports_first_broken_step(self) -> None:
        case, recording_path, _, _ = self.sample("case-02-v1-upgrade")
        del case
        recording = load_recording(recording_path)
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="fixed",
                config_digest=recording.config_digest(),
            )
            run_recording(recording, "fixed", snapshot_store=store, cancel_after=2)
            path = os.path.join(directory, "step-000002.json")
            with open(path, "r", encoding="utf-8") as handle:
                payload = json.load(handle)
            payload.pop("error_state")
            atomic_write_json(path, payload)
            with self.assertRaises(ReplayError) as caught:
                store.load_latest()
            self.assertEqual(caught.exception.code, "E_RECOVERY_INVALID")
            self.assertEqual(caught.exception.step, 2)
            self.assertEqual(caught.exception.path, "snapshot.error_state")

    def test_hash_chain_tamper_reports_first_broken_step(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="fixed",
                config_digest=recording.config_digest(),
            )
            run_recording(recording, "fixed", snapshot_store=store, cancel_after=3)
            path = os.path.join(directory, "step-000002.json")
            with open(path, "r", encoding="utf-8") as handle:
                payload = json.load(handle)
            payload["hash_chain_prefix"] = "1" * 64
            atomic_write_json(path, payload)
            with self.assertRaises(ReplayError) as caught:
                store.load_latest()
            self.assertEqual(caught.exception.code, "E_RECOVERY_INVALID")
            self.assertEqual(caught.exception.step, 2)
            self.assertEqual(caught.exception.path, "snapshot.hash_chain_prefix")


class BaselineTests(SampleFixture):
    def test_generated_baseline_matches(self) -> None:
        case, recording_path, baseline_path, _ = self.sample("case-01-v2-mixed")
        recording = load_recording(recording_path)
        run = run_recording(recording, "fixed")
        report = compare_baseline(run, baseline_path)
        self.assertTrue(report["ok"])
        self.assertIsNone(report["first_mismatch"])

    def test_mismatch_reports_field_and_does_not_infer_without_ops(self) -> None:
        case, recording_path, _, mismatch_path = self.sample("case-01-v2-mixed")
        del case
        recording = load_recording(recording_path)
        run = run_recording(recording, "fixed")
        report = compare_baseline(run, mismatch_path)
        self.assertFalse(report["ok"])
        mismatch = report["first_mismatch"]
        self.assertIn("alive", mismatch["field"])
        self.assertFalse(mismatch["attribution_available"])
        self.assertEqual(mismatch["fork_causes"], [])
        self.assertIn("not inferred", mismatch["attribution_note"])
        self.assertEqual(mismatch["mode"], "fixed")
        self.assertIsInstance(mismatch["effective_ops"], list)

    def test_baseline_version_mismatch(self) -> None:
        case, recording_path, baseline_path, _ = self.sample("case-01-v2-mixed")
        with open(baseline_path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
        payload["state_model_version"] = "state-v1"
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as handle:
            json.dump(payload, handle)
            path = handle.name
        try:
            recording = load_recording(recording_path)
            run = run_recording(recording, "fixed")
            with self.assertRaises(ReplayError) as caught:
                compare_baseline(run, path)
            self.assertEqual(caught.exception.code, "E_BASELINE_VERSION")
        finally:
            os.unlink(path)

    def test_corrupt_baseline_injection(self) -> None:
        case, recording_path, baseline_path, _ = self.sample("case-01-v2-mixed")
        recording = load_recording(recording_path)
        run = run_recording(recording, "fixed")
        with self.assertRaises(ReplayError) as caught:
            compare_baseline(run, baseline_path, inject=["corrupt_baseline"])
        self.assertEqual(caught.exception.code, "E_BASELINE_CORRUPT")
        self.assertEqual(caught.exception.path, "frames[0].state_hash")
        self.assertEqual(
            caught.exception.injection["point"],
            "frames[0].state_hash",
        )


class InjectionAndConcurrencyTests(SampleFixture):
    def test_recording_injections_have_distinct_codes(self) -> None:
        case, recording_path, _, _ = self.sample("case-01-v2-mixed")
        del case
        expected = {
            "dup_client_seq": "E_DUP_CLIENT_SEQ",
            "out_of_bounds_coord": "E_COORD_RANGE",
            "nonfinite_heading": "E_NONFINITE",
            "late_operation": "E_LATE_OP",
        }
        for name, code in expected.items():
            with self.subTest(name=name):
                with self.assertRaises(ReplayError) as caught:
                    load_recording(recording_path, inject=[name])
                self.assertEqual(caught.exception.code, code)
                self.assertIsNotNone(caught.exception.injection)
                self.assertTrue(caught.exception.injection["point"])

    def test_match_lock_is_exclusive(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            first = MatchLock(directory, "match-minimal")
            first.acquire()
            try:
                with self.assertRaises(ReplayError) as caught:
                    MatchLock(directory, "match-minimal").acquire()
                self.assertEqual(caught.exception.code, "E_BUSY")
            finally:
                first.release()
            second = MatchLock(directory, "match-minimal")
            second.acquire()
            second.release()

    def test_stale_lock_from_dead_process_is_reclaimed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "match-minimal.lock")
            with open(path, "w", encoding="ascii") as handle:
                handle.write("pid=99999999\n")
            lock = MatchLock(directory, "match-minimal")
            lock.acquire()
            lock.release()

    def test_cancel_after_keeps_last_committed_snapshot(self) -> None:
        recording = parse_recording(_minimal_raw(total_steps=8))
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="fixed",
                config_digest=recording.config_digest(),
            )
            run = run_recording(recording, "fixed", snapshot_store=store, cancel_after=2)
            self.assertTrue(run.canceled)
            self.assertEqual(run.last_committed_step, 2)
            self.assertEqual(
                [step for step, _ in store.snapshot_files()],
                [1, 2],
            )
            self.assertEqual(store.load_latest().step, 2)

    def test_truncated_snapshot_injection_code(self) -> None:
        recording = parse_recording(_minimal_raw(total_steps=5))
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(
                directory,
                match_id=recording.match_id,
                mode="fixed",
                config_digest=recording.config_digest(),
            )
            run_recording(recording, "fixed", snapshot_store=store, cancel_after=2)
            from replay.injection import truncate_latest_snapshot

            context = truncate_latest_snapshot(directory)
            self.assertEqual(context["name"], "truncated_snapshot")
            with self.assertRaises(ReplayError) as caught:
                store.load_latest()
            self.assertEqual(caught.exception.code, "E_RECOVERY_INVALID")
            self.assertEqual(caught.exception.step, 2)


if __name__ == "__main__":
    unittest.main()
