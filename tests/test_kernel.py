from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from causalkernel.canonical import atomic_write_json, canonical_bytes, hash_object, parse_json_bytes
from causalkernel.engine import (
    Engine,
    EvalNode,
    _attribute_uncertainty,
    replay_trace,
)
from causalkernel.errors import KernelError
from causalkernel.precision import Precision
from causalkernel.samples import all_samples, generate_samples
from causalkernel.scenario import load_scenario, parse_scenario


class KernelTestCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name)
        cls.data = cls.root / "data"
        cls.reports = cls.root / "reports"
        cls.data.mkdir()
        cls.reports.mkdir()
        generate_samples(cls.data)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_fixed_and_float_branch_flip_with_attribution(self) -> None:
        scenario = load_scenario(self.data / "branch_flip.json")
        fixed = Engine(
            scenario,
            Precision("fixed", 3),
            operations=["b_edge"],
        ).run()
        floating = Engine(
            scenario,
            Precision("float"),
            operations=["b_edge"],
        ).run()
        self.assertEqual(fixed.steps[-1]["selected_event"], "edge")
        self.assertEqual(fixed.steps[-1]["selected_branch"], "b_edge")
        self.assertEqual(
            fixed.final_state["characters.hero.trust"]["value"]["value"],
            "1.05",
        )
        self.assertEqual(floating.steps[-1]["selected_event"], "fallback")
        self.assertEqual(floating.steps[-1]["selected_branch"], "b_fallback")
        ratio = next(
            record
            for step in fixed.steps
            for record in step["evaluated_conditions"]
            if record["condition_id"] == "ratio_ok"
            and record["purpose"] == "branch"
        )
        self.assertTrue(ratio["truth"])
        self.assertTrue(ratio["uncertain_branch"])
        self.assertLessEqual(
            ratio["lower_bound"]["value"],
            ratio["threshold"]["value"]["value"],
        )
        self.assertGreaterEqual(
            ratio["upper_bound"]["value"],
            ratio["threshold"]["value"]["value"],
        )
        attribution = ratio["attribution"]
        self.assertEqual(attribution["status"], "attributed")
        self.assertTrue(
            attribution["earliest_factor"]["node_id"].endswith(".expr.base")
        )
        self.assertEqual(
            attribution["earliest_factor"]["factor_kind"],
            "coefficient_quantization",
        )

    def test_step_records_effects_and_state_hashes(self) -> None:
        scenario = load_scenario(self.data / "branch_flip.json")
        result = Engine(scenario, Precision("fixed", 3)).run()
        entry = result.steps[0]
        self.assertEqual(entry["kind"], "entry")
        self.assertEqual(entry["selected_event"], "start")
        self.assertNotEqual(entry["state_before_hash"], entry["state_after_hash"])
        self.assertEqual(entry["state_after"]["flags.gate_open"]["value"], True)
        for step in result.steps:
            body = dict(step)
            expected = body.pop("record_hash")
            self.assertEqual(hash_object(body), expected)
            self.assertEqual(
                step["state_after"],
                body["state_after"],
            )
        self.assertEqual(
            result.steps[1]["prev_hash"],
            result.steps[0]["record_hash"],
        )

    def test_priority_then_declaration_order_is_stable(self) -> None:
        document = {
            "format": "causal-story/1",
            "scenario_id": "priority",
            "start_event": "start",
            "precision": {"mode": "fixed", "decimal_places": 3},
            "initial_state": {"flags": {"done": False}},
            "events": [
                {
                    "id": "start",
                    "preconditions": [],
                    "conditions": [
                        {
                            "id": "always",
                            "expr": {
                                "op": "const",
                                "coef": {"kind": "fixed", "text": "1", "scale": 0},
                            },
                            "op": ">=",
                            "threshold": {"kind": "fixed", "text": "0", "scale": 0},
                        }
                    ],
                    "effects": [],
                    "branches": [
                        {
                            "id": "first_low",
                            "to": "low",
                            "priority": 1,
                            "condition": "always",
                        },
                        {
                            "id": "second_high",
                            "to": "high",
                            "priority": 10,
                            "condition": "always",
                        },
                        {
                            "id": "third_high",
                            "to": "high",
                            "priority": 10,
                            "condition": "always",
                        },
                    ],
                },
                {
                    "id": "low",
                    "preconditions": [],
                    "conditions": [],
                    "effects": [],
                    "branches": [],
                },
                {
                    "id": "high",
                    "preconditions": [],
                    "conditions": [],
                    "effects": [],
                    "branches": [],
                },
            ],
        }
        scenario = parse_scenario(document)
        result = Engine(scenario, Precision("fixed", 3)).run()
        self.assertEqual(result.steps[1]["selected_branch"], "second_high")

    def test_byte_identical_traces(self) -> None:
        scenario = load_scenario(self.data / "branch_flip.json")
        first = Engine(
            scenario,
            Precision("fixed", 3),
            operations=["b_edge"],
        ).run()
        second = Engine(
            scenario,
            Precision("fixed", 3),
            operations=["b_edge"],
        ).run()
        first_path = self.reports / "first.json"
        second_path = self.reports / "second.json"
        first.write_trace(first_path)
        second.write_trace(second_path)
        self.assertEqual(first_path.read_bytes(), second_path.read_bytes())
        self.assertEqual(
            first.build_trace()["trace_hash"],
            second.build_trace()["trace_hash"],
        )

    def test_trace_replay_and_integrity_failures(self) -> None:
        scenario_path = self.data / "branch_flip.json"
        scenario = load_scenario(scenario_path)
        result = Engine(
            scenario,
            Precision("fixed", 3),
            operations=["b_edge"],
        ).run()
        trace_path = self.reports / "valid.trace.json"
        result.write_trace(trace_path)
        replay = replay_trace(scenario_path, trace_path)
        self.assertEqual(replay.final_state_hash, result.final_state_hash)
        self.assertEqual(replay.steps, result.steps)

        trace = parse_json_bytes(trace_path.read_bytes())
        trace["steps"][0]["state_after_hash"] = "0" * 64
        tampered_path = self.reports / "tampered.trace.json"
        atomic_write_json(tampered_path, trace)
        with self.assertRaises(KernelError) as tampered:
            replay_trace(scenario_path, tampered_path)
        self.assertEqual(tampered.exception.code, "E_TRACE_INVALID")

        truncated_path = self.reports / "truncated.trace.json"
        truncated_path.write_bytes(trace_path.read_bytes()[:200])
        with self.assertRaises(KernelError) as truncated:
            replay_trace(scenario_path, truncated_path)
        self.assertEqual(truncated.exception.code, "E_TRACE_INVALID")

    def test_all_injection_codes_and_no_trace(self) -> None:
        expected = {
            "cycle": "E_INJECT_CYCLE",
            "precision-conflict": "E_INJECT_PRECISION_CONFLICT",
            "deep-chain": "E_INJECT_DEPTH_EXCEEDED",
            "nonfinite": "E_INJECT_NONFINITE_COEFFICIENT",
            "truncated": "E_INJECT_TRUNCATED",
        }
        for injection, code in expected.items():
            trace_path = self.reports / f"{injection}.trace.json"
            if trace_path.exists():
                trace_path.unlink()
            with self.assertRaises(KernelError) as error:
                load_scenario(self.data / "branch_flip.json", injection=injection)
            self.assertEqual(error.exception.code, code)
            self.assertTrue(error.exception.location)
            self.assertFalse(trace_path.exists())

    def test_scenario_validation_errors(self) -> None:
        documents = all_samples()
        base = documents["branch_flip.json"]

        missing_target = json.loads(json.dumps(base))
        missing_target["events"][0]["branches"][0]["to"] = "does_not_exist"
        with self.assertRaises(KernelError) as dangling:
            parse_scenario(missing_target)
        self.assertEqual(dangling.exception.code, "E_SCENARIO_INVALID")
        self.assertEqual(
            dangling.exception.location["target"],
            "does_not_exist",
        )

        missing_start = json.loads(json.dumps(base))
        missing_start["start_event"] = "missing"
        with self.assertRaises(KernelError) as start:
            parse_scenario(missing_start)
        self.assertEqual(start.exception.code, "E_SCENARIO_INVALID")

        illegal_coefficient = json.loads(json.dumps(base))
        illegal_coefficient["events"][0]["conditions"][0]["threshold"] = {
            "kind": "fixed",
            "text": "NaN",
            "scale": 3,
        }
        with self.assertRaises(KernelError) as coefficient:
            parse_scenario(illegal_coefficient)
        self.assertEqual(coefficient.exception.code, "E_SCENARIO_INVALID")

        cycle = json.loads(json.dumps(base))
        cycle["events"][1]["branches"] = [
            {
                "id": "cycle_branch",
                "to": "start",
                "priority": 0,
                "condition": None,
            }
        ]
        with self.assertRaises(KernelError) as cyclic:
            parse_scenario(cycle)
        self.assertEqual(cyclic.exception.code, "E_SCENARIO_INVALID")
        self.assertIn("cycle", cyclic.exception.location)

    def test_attribution_failure_when_no_error_factor(self) -> None:
        root = EvalNode(
            node_id="root",
            op="const",
            point=0.5,
            lower=0.4,
            upper=0.6,
            local_error=0.0,
            local_kind="coefficient_quantization",
            rounding_direction="none",
        )
        with self.assertRaises(KernelError) as error:
            _attribute_uncertainty(
                [root],
                root,
                0.5,
                "event",
                "condition",
            )
        self.assertEqual(error.exception.code, "E_ATTRIBUTION_FAILED")

    def test_snapshot_forced_crash_and_recovery(self) -> None:
        scenario_path = self.data / "deterministic_small.json"
        snapshot_dir = self.root / "snapshots"
        recovered_trace = self.root / "recovered.trace.json"
        baseline_trace = self.root / "baseline.trace.json"
        common = [
            sys.executable,
            "-m",
            "causalkernel.cli",
            "run",
            "--scenario",
            str(scenario_path),
            "--mode",
            "fixed",
            "--places",
            "4",
            "--max-steps",
            "5",
            "--snapshot-dir",
            str(snapshot_dir),
        ]
        crash = subprocess.run(
            common + ["--crash-after", "2"],
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(crash.returncode, 17)
        self.assertEqual(len(list(snapshot_dir.glob("snapshot-*.json"))), 2)
        recovery = subprocess.run(
            common + ["--recover", "--trace", str(recovered_trace)],
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(recovery.returncode, 0, recovery.stdout + recovery.stderr)
        baseline = subprocess.run(
            common + ["--trace", str(baseline_trace)],
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(baseline.returncode, 0, baseline.stdout + baseline.stderr)
        recovered = parse_json_bytes(recovered_trace.read_bytes())
        expected = parse_json_bytes(baseline_trace.read_bytes())
        self.assertEqual(recovered["steps"], expected["steps"])
        self.assertEqual(recovered["final_state_hash"], expected["final_state_hash"])
        self.assertGreater(len(recovered["steps"]), 2)

        latest = sorted(snapshot_dir.glob("snapshot-*.json"))[-1]
        corrupt = parse_json_bytes(latest.read_bytes())
        corrupt["version"] = 999
        corrupt.pop("snapshot_hash", None)
        corrupt["snapshot_hash"] = hash_object(corrupt)
        atomic_write_json(latest, corrupt)
        scenario = load_scenario(scenario_path)
        with self.assertRaises(KernelError) as version_error:
            Engine.recover(
                scenario,
                Precision("fixed", 4),
                snapshot_dir,
                max_steps=5,
            )
        self.assertEqual(version_error.exception.code, "E_RECOVERY_INVALID")

    def test_atomic_write_has_no_temp_residue(self) -> None:
        output = self.root / "atomic"
        output.mkdir()
        path = output / "payload.json"
        payload = {"format": "test", "value": 1}
        atomic_write_json(path, payload)
        self.assertEqual(parse_json_bytes(path.read_bytes()), payload)
        self.assertEqual(list(output.glob(".*.tmp")), [])


if __name__ == "__main__":
    unittest.main()
