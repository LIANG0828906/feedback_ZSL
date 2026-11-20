from __future__ import annotations

import copy
import json
import unittest
from pathlib import Path
from unittest.mock import patch

from levelworkbench.config import load_config, parse_config_document
from levelworkbench.errors import (
    E_CONSTRAINT_CONFLICT,
    E_GENERATION_FAILED,
    E_TRACE_INVALID,
    WorkbenchError,
)
from levelworkbench.fixed import build_area_model, model_result, quantize
from levelworkbench.models import Door, Level, Room, Tag
from levelworkbench.pipeline import generate_from_config, generate_from_document, replay_trace
from levelworkbench.reachability import analyze_key_chain
from levelworkbench.rng import derive_seed
from levelworkbench.session import TaskGate
from levelworkbench.trace import load_trace_document


ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


class SampleGenerationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.config = load_config(SAMPLES / "config.json")
        cls.result = generate_from_config(cls.config)

    def test_hard_constraints_and_reachability_are_consistent(self) -> None:
        self.assertFalse(self.result.evaluation.failed_hard)
        direct = analyze_key_chain(
            self.result.level,
            self.result.evaluation.reachability.required_keys,
        )
        self.assertEqual(
            direct.to_dict(),
            self.result.evaluation.reachability.to_dict(),
        )
        self.assertTrue(direct.ok)
        self.assertTrue(direct.end_reachable)
        self.assertFalse(direct.end_reachable_without_locks)
        self.assertFalse(direct.premature_end_reachable)
        self.assertEqual(
            list(direct.unlock_order),
            list(self.result.evaluation.reachability.required_keys),
        )
        self.assertGreaterEqual(len(direct.path), 2)

    def test_determinism_and_constraint_derived_seed(self) -> None:
        again = generate_from_config(self.config)
        self.assertEqual(self.result.layout_hash, again.layout_hash)
        self.assertEqual(self.result.trace_hash, again.trace_hash)
        self.assertEqual(self.result.seed_derivation_hash, again.seed_derivation_hash)

        changed_document = copy.deepcopy(self.config.raw_document)
        for item in changed_document["constraints"]:
            if item["type"] == "room_count":
                item["max"] = item["max"] - 1
        changed = parse_config_document(changed_document)
        params = {
            "phases": [
                "room_layout",
                "door_assignment",
                "tag_placement",
                "key_chain",
            ],
            "generation": changed.to_dict().get("generation", {}),
            "quantization": changed.to_dict().get("quantization", {}),
            "evaluation": changed.to_dict().get("evaluation", {}),
        }
        changed_seed = derive_seed(
            changed.seed,
            [item.to_dict() for item in changed.constraints],
            params,
        )
        original_seed = derive_seed(
            self.config.seed,
            [item.to_dict() for item in self.config.constraints],
            {
                "phases": [
                    "room_layout",
                    "door_assignment",
                    "tag_placement",
                    "key_chain",
                ],
                "generation": self.config.to_dict().get("generation", {}),
                "quantization": self.config.to_dict().get("quantization", {}),
                "evaluation": self.config.to_dict().get("evaluation", {}),
            },
        )
        self.assertNotEqual(original_seed, changed_seed)

    def test_trace_replay_verifies_hashes(self) -> None:
        replay = replay_trace(self.result.trace)
        self.assertTrue(replay.layout_hash_verified)
        self.assertTrue(replay.attribution_hash_verified)
        self.assertTrue(replay.event_chain_verified)
        self.assertEqual(replay.source_trace_hash, self.result.trace_hash)

    def test_replay_after_phase_rollback(self) -> None:
        from levelworkbench.generator import DeterministicLevelGenerator, _PhaseFailure

        original = DeterministicLevelGenerator._phase_room_layout

        def fail_first_retry(self, state, attempt, retry):
            if attempt == 0 and retry == 0:
                raise _PhaseFailure(
                    stage="room_layout",
                    failure_id="forced_rollback",
                    reason="forced rollback for replay coverage",
                    constraint_ids=["rooms"],
                    minimal_evidence={"constraints": ["rooms"], "facts": []},
                    checks=[],
                )
            return original(self, state, attempt, retry)

        with patch.object(
            DeterministicLevelGenerator,
            "_phase_room_layout",
            fail_first_retry,
        ):
            result = generate_from_config(self.config)
            self.assertTrue(
                any(
                    event["kind"] == "phase_rollback"
                    for event in result.trace["events"]
                )
            )
            replay = replay_trace(result.trace)
            self.assertTrue(replay.event_chain_verified)
            self.assertTrue(replay.layout_hash_verified)
            self.assertTrue(replay.attribution_hash_verified)

    def test_trace_rejects_truncation_hash_break_and_missing_version(self) -> None:
        truncated = copy.deepcopy(self.result.trace)
        truncated["events"] = truncated["events"][:-1]
        with self.assertRaises(WorkbenchError) as caught:
            load_trace_document(document=truncated)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)

        broken = copy.deepcopy(self.result.trace)
        broken["events"][2]["payload"]["tampered"] = True
        with self.assertRaises(WorkbenchError) as caught:
            load_trace_document(document=broken)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)

        missing_version = copy.deepcopy(self.result.trace)
        missing_version["meta"].pop("trace_version")
        with self.assertRaises(WorkbenchError) as caught:
            load_trace_document(document=missing_version)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)

    def test_samples_match_baselines(self) -> None:
        baselines = json.loads((SAMPLES / "baselines.json").read_text(encoding="utf-8"))
        for name, expected in baselines["samples"].items():
            if name.startswith("conflict_"):
                self.assertEqual(expected["expected_error"], E_CONSTRAINT_CONFLICT)
                continue
            document = json.loads((SAMPLES / name).read_text(encoding="utf-8"))
            result = generate_from_document(document)
            self.assertEqual(result.layout_hash, expected["layout_hash"], name)
            self.assertEqual(result.trace_hash, expected["trace_hash"], name)
            self.assertEqual(
                result.seed_derivation_hash,
                expected["seed_derivation_hash"],
                name,
            )


class ReachabilityTests(unittest.TestCase):
    @staticmethod
    def _level(key_room: str) -> Level:
        return Level(
            grid_width=12,
            grid_height=4,
            rooms=(
                Room("r0", 0, 0, 3, 3),
                Room("r1", 4, 0, 3, 3),
                Room("r2", 8, 0, 3, 3),
            ),
            doors=(
                Door("d0", "r0", "r1", "both", locked_by="k1"),
                Door("d1", "r1", "r2", "both"),
            ),
            tags=(
                Tag("s", "start", "r0"),
                Tag("e", "end", "r2"),
                Tag("key", "key", key_room, key_id="k1"),
            ),
        )

    def test_locked_door_cannot_be_crossed_before_key(self) -> None:
        result = analyze_key_chain(self._level("r2"), ["k1"])
        self.assertFalse(result.ok)
        self.assertFalse(result.end_reachable)
        self.assertIn("k1", result.missing_keys)
        self.assertEqual(result.unlock_order, ())

    def test_key_before_lock_unlocks_and_reports_path(self) -> None:
        result = analyze_key_chain(self._level("r0"), ["k1"])
        self.assertTrue(result.ok)
        self.assertEqual(result.path, ("r0", "r1", "r2"))
        self.assertEqual(result.unlock_order, ("k1",))
        self.assertFalse(result.end_reachable_without_locks)


class FixedPointTests(unittest.TestCase):
    def test_quantization_direction(self) -> None:
        down = quantize(0.333, 2, "half_up")
        up = quantize(0.335, 2, "half_up")
        self.assertEqual(down.direction, "down")
        self.assertEqual(up.direction, "up")
        self.assertEqual(str(down.quantized), "0.33")
        self.assertEqual(str(up.quantized), "0.34")

    def test_interval_crosses_threshold_and_attributes_coefficient(self) -> None:
        model = build_area_model([9], 2.997, 0.0, 0.333, 2, "half_up")
        result = model_result(model, "area.compare", "area_quantized")
        self.assertFalse(result["fixed_truth"])
        self.assertTrue(result["float_truth"])
        self.assertFalse(result["truth_lower"])
        self.assertTrue(result["truth_upper"])
        self.assertTrue(result["uncertain"])
        self.assertEqual(result["attribution"]["first_factor"], "area coefficient")
        self.assertEqual(result["attribution"]["node_id"], "area.coefficient")
        self.assertEqual(result["attribution"]["quantization"]["direction"], "down")
        operations = {node["op"] for node in result["nodes"]}
        self.assertTrue({"sum", "product", "subtract", "abs", "compare"} <= operations)

    def test_quantized_probe_is_reported_end_to_end(self) -> None:
        document = json.loads((SAMPLES / "quantized_probe.json").read_text(encoding="utf-8"))
        result = generate_from_document(document)
        self.assertFalse(result.evaluation.failed_hard)
        self.assertEqual(len(result.evaluation.error_report), 1)
        report = result.evaluation.error_report[0]
        self.assertEqual(report["constraint_id"], "area_quantized")
        self.assertFalse(report["fixed_truth"])
        self.assertTrue(report["float_truth"])
        self.assertTrue(report["uncertain"])
        self.assertFalse(report["truth_lower"])
        self.assertTrue(report["truth_upper"])
        self.assertEqual(report["quantization"]["decimals"], 2)
        self.assertEqual(
            report["first_uncertain_factor"]["quantization"]["direction"],
            "down",
        )


class ConflictTests(unittest.TestCase):
    def test_room_count_vs_key_chain_conflict_has_minimal_evidence(self) -> None:
        document = json.loads(
            (SAMPLES / "conflict_rooms_vs_chain.json").read_text(encoding="utf-8")
        )
        with self.assertRaises(WorkbenchError) as caught:
            generate_from_document(document)
        error = caught.exception
        self.assertEqual(error.code, E_CONSTRAINT_CONFLICT)
        self.assertEqual(error.stage, "room_layout")
        self.assertEqual(
            error.details["violated_constraints"],
            ["rooms", "key_chain"],
        )
        evidence = error.details["minimal_evidence"]
        self.assertEqual(evidence["constraints"], ["rooms", "key_chain"])
        self.assertTrue(any("required_rooms=3" in fact for fact in evidence["facts"]))

    def test_quantized_hard_failure_reports_stage_and_constraint(self) -> None:
        document = json.loads((SAMPLES / "small.json").read_text(encoding="utf-8"))
        document["generation"]["max_attempts"] = 2
        document["generation"]["max_phase_retries"] = 2
        for item in document["constraints"]:
            if item["type"] == "key_chain":
                item["keys"] = ["k1", "k2"]
                item["coefficients"] = {"k1": 0.333, "k2": 0.333}
                item["threshold"] = 0.11
        with self.assertRaises(WorkbenchError) as caught:
            generate_from_document(document)
        error = caught.exception
        self.assertEqual(error.code, E_GENERATION_FAILED)
        self.assertEqual(error.stage, "key_chain")
        self.assertIn("key_chain", error.details["violated_constraints"])
        self.assertIsNotNone(error.details["minimal_evidence"])
        checks = error.details["checks"]
        chain = next(item for item in checks if item["constraint_id"] == "key_chain")
        self.assertFalse(chain["fixed_truth"])
        self.assertTrue(chain["float_truth"])
        self.assertEqual(chain["attribution"]["node_id"], "chain.factor.k0")


class ConcurrencyTests(unittest.TestCase):
    def test_task_gate_returns_busy_for_second_task(self) -> None:
        gate = TaskGate()
        self.assertTrue(gate.try_acquire())
        self.assertFalse(gate.try_acquire())
        with self.assertRaises(WorkbenchError) as caught:
            gate.acquire_or_raise()
        self.assertEqual(caught.exception.code, "E_BUSY")
        gate.release()
        self.assertTrue(gate.try_acquire())
        gate.release()

    def test_pre_cancelled_generation_commits_nothing(self) -> None:
        import threading

        event = threading.Event()
        event.set()
        config = load_config(SAMPLES / "small.json")
        with self.assertRaises(WorkbenchError) as caught:
            generate_from_config(config, cancel_event=event)
        self.assertEqual(caught.exception.code, "E_CANCELLED")


if __name__ == "__main__":
    unittest.main()
