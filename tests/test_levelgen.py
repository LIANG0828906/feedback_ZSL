"""Local regressions for deterministic generation, diagnosis and injection."""

from __future__ import annotations

import copy
import json
import os
import tempfile
import threading
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from levelgen.canonical import dump_json_bytes
from levelgen.errors import (
    E_BUSY,
    E_CANCELLED,
    E_CONSTRAINT_CONFLICT,
    E_DOOR_DANGLING,
    E_INPUT_TRUNCATED,
    E_KEY_CYCLE,
    E_ROOM_OUT_OF_BOUNDS,
    E_TRACE_INVALID,
    LevelGenError,
)
from levelgen.generator import generate
from levelgen.injection import INJECTION_TYPES, run_injection
from levelgen.reachability import analyze_reachability
from levelgen.service import run_generation_to_directory, run_replay_to_directory
from levelgen.trace import replay_trace, trace_content_hash, validate_trace
from levelgen.workbench import SingleTaskGate


ROOT = Path(__file__).resolve().parents[1]
REQUEST = json.loads(
    (ROOT / "artifacts" / "requests" / "s24_std.request.json").read_text(
        encoding="utf-8"
    )
)


class LevelGenerationTests(unittest.TestCase):
    def test_same_input_is_field_and_byte_identical(self) -> None:
        first = generate(REQUEST)
        second = generate(copy.deepcopy(REQUEST))
        self.assertTrue(first.ok)
        self.assertTrue(second.ok)
        self.assertIsNotNone(first.level)
        self.assertIsNotNone(second.level)
        self.assertIsNotNone(first.trace)
        self.assertIsNotNone(second.trace)
        self.assertEqual(first.level.to_dict(), second.level.to_dict())
        self.assertEqual(
            dump_json_bytes(first.level.to_dict()),
            dump_json_bytes(second.level.to_dict()),
        )
        self.assertEqual(
            dump_json_bytes(first.trace),
            dump_json_bytes(second.trace),
        )
        self.assertEqual(first.level.content_hash, second.level.content_hash)

    def test_unrelated_constraint_addition_does_not_shift_level_draws(self) -> None:
        base = copy.deepcopy(REQUEST)
        base["constraints"] = [
            constraint
            for constraint in base["constraints"]
            if constraint["kind"] != "area_target"
        ]
        with_area = copy.deepcopy(base)
        with_area["constraints"].append(
            {
                "hard": False,
                "id": "c_extra_area",
                "kind": "area_target",
                "params": {"target": 120, "tolerance": 80},
            }
        )
        first = generate(base)
        second = generate(with_area)
        self.assertTrue(first.ok)
        self.assertTrue(second.ok)
        self.assertEqual(first.level.content_hash, second.level.content_hash)
        first_draws = [
            {
                "method": entry["method"],
                "args": entry["args"],
                "result": entry["result"],
                "stream": entry["stream"],
            }
            for entry in first.trace["entries"]
            if entry["type"] == "rng"
        ]
        second_draws = [
            {
                "method": entry["method"],
                "args": entry["args"],
                "result": entry["result"],
                "stream": entry["stream"],
            }
            for entry in second.trace["entries"]
            if entry["type"] == "rng"
        ]
        self.assertEqual(first_draws, second_draws)

    def test_replay_asserts_final_hash_and_rejects_broken_chain(self) -> None:
        result = generate(REQUEST)
        self.assertTrue(result.ok)
        self.assertIsNotNone(result.trace)
        self.assertIsNotNone(result.level)
        replay = replay_trace(result.trace)
        self.assertTrue(replay.ok)
        self.assertEqual(replay.level.content_hash, result.level.content_hash)

        with tempfile.TemporaryDirectory() as temporary:
            trace_path = Path(temporary) / "valid.trace.json"
            output_path = Path(temporary) / "out"
            trace_path.write_bytes(dump_json_bytes(result.trace))
            payload = run_replay_to_directory(trace_path, output_path)
            self.assertTrue(payload["ok"])
            self.assertTrue((output_path / "level.json").exists())

        damaged = copy.deepcopy(result.trace)
        damaged["entries"][8]["result"] = 123456
        with self.assertRaises(LevelGenError) as raised:
            replay_trace(damaged)
        self.assertEqual(raised.exception.code, E_TRACE_INVALID)

        with tempfile.TemporaryDirectory() as temporary:
            trace_path = Path(temporary) / "damaged.trace.json"
            output_path = Path(temporary) / "out"
            trace_path.write_bytes(dump_json_bytes(damaged))
            with self.assertRaises(LevelGenError):
                run_replay_to_directory(trace_path, output_path)
            self.assertFalse((output_path / "level.json").exists())

        with tempfile.TemporaryDirectory() as temporary:
            trace_path = Path(temporary) / "broken.trace.json"
            output_path = Path(temporary) / "out"
            trace_path.write_text('{"entries": [', encoding="utf-8")
            with self.assertRaises(LevelGenError) as raised:
                run_replay_to_directory(trace_path, output_path)
            self.assertEqual(raised.exception.code, E_TRACE_INVALID)
            self.assertFalse((output_path / "level.json").exists())

    def test_reachability_is_shared_and_reports_unlock_order(self) -> None:
        result = generate(REQUEST)
        self.assertTrue(result.ok)
        analysis = analyze_reachability(result.level, ["K1", "K2"])
        self.assertFalse(analysis["no_lock"]["end_reachable"])
        self.assertTrue(analysis["end_after_all_keys"])
        self.assertEqual(analysis["full"]["unlock_order"], ["K1", "K2"])
        self.assertTrue(analysis["full"]["room_sequence"])
        self.assertTrue(analysis["full"]["door_sequence"])
        self.assertTrue(all(stage["ok"] for stage in analysis["stages"]))

    def test_conflicts_have_stage_ids_and_reconstructable_trace(self) -> None:
        conflicts = {
            "conflict_room_range": ["c_rooms_a", "c_rooms_b"],
            "conflict_chain_vs_rooms": ["c_chain", "c_rooms"],
            "conflict_end_without_chain": ["c_end"],
        }
        for name, expected_ids in conflicts.items():
            request = json.loads(
                (ROOT / "artifacts" / "requests" / f"{name}.request.json").read_text(
                    encoding="utf-8"
                )
            )
            result = generate(request)
            self.assertFalse(result.ok)
            self.assertEqual(result.code, E_CONSTRAINT_CONFLICT)
            self.assertIsNotNone(result.conflict)
            self.assertEqual(result.conflict["constraint_ids"], expected_ids)
            self.assertEqual(result.conflict["stage"], "static")
            replay = replay_trace(result.trace)
            self.assertFalse(replay.ok)
            self.assertEqual(replay.conflict, result.conflict)

    def test_stage_failure_rolls_back_to_snapshot(self) -> None:
        impossible_layout = {
            "constraints": [
                {
                    "hard": True,
                    "id": "c_rooms",
                    "kind": "room_count",
                    "params": {"max": 4, "min": 4},
                },
                {
                    "hard": True,
                    "id": "c_conn",
                    "kind": "connectivity",
                    "params": {},
                },
            ],
            "params": {
                "grid_h": 5,
                "grid_w": 5,
                "max_attempts": 2,
                "rewards": 0,
                "room_max_h": 4,
                "room_max_w": 4,
                "room_min_h": 4,
                "room_min_w": 4,
            },
            "seed": 1,
        }
        result = generate(impossible_layout)
        self.assertFalse(result.ok)
        types = [entry["type"] for entry in result.trace["entries"]]
        self.assertIn("stage_failure", types)
        self.assertIn("stage_rollback", types)
        rollback_entry = next(
            entry
            for entry in result.trace["entries"]
            if entry["type"] == "stage_rollback"
        )
        self.assertEqual(rollback_entry["stage"], "layout")
        self.assertIn("restored_hash", rollback_entry)

    def test_all_injections_return_independent_codes_before_level(self) -> None:
        expected = {
            "constraint_limit": "E_CONSTRAINT_LIMIT",
            "dangling_door": E_DOOR_DANGLING,
            "key_lock_cycle": E_KEY_CYCLE,
            "room_out_of_bounds": E_ROOM_OUT_OF_BOUNDS,
            "truncated_input": E_INPUT_TRUNCATED,
        }
        self.assertEqual(set(expected), set(INJECTION_TYPES))
        for injection_type, code in expected.items():
            with self.subTest(injection_type=injection_type):
                result = run_injection(REQUEST, injection_type)
                self.assertEqual(result.code, code)
                self.assertTrue(result.point)
                self.assertFalse(result.level_written)
                self.assertFalse(result.ok)

    def test_cancellation_returns_no_partial_level(self) -> None:
        cancel_event = threading.Event()
        cancel_event.set()
        result = generate(REQUEST, cancel_event=cancel_event)
        self.assertFalse(result.ok)
        self.assertEqual(result.code, E_CANCELLED)
        self.assertIsNone(result.level)

    def test_failed_generation_does_not_leave_level_file(self) -> None:
        conflict_request = json.loads(
            (
                ROOT
                / "artifacts"
                / "requests"
                / "conflict_room_range.request.json"
            ).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "out"
            output.mkdir()
            (output / "level.json").write_text("stale", encoding="utf-8")
            result = run_generation_to_directory(conflict_request, output)
            self.assertFalse(result.ok)
            self.assertFalse((output / "level.json").exists())
            self.assertTrue((output / "conflict.json").exists())

    def test_single_task_gate_rejects_second_task_with_busy(self) -> None:
        gate = SingleTaskGate()
        self.assertIsNone(gate.try_start())
        self.assertTrue(gate.active)
        self.assertEqual(gate.try_start(), E_BUSY)
        gate.finish()
        self.assertFalse(gate.active)
        self.assertIsNone(gate.try_start())

    def test_gui_module_import_is_display_independent(self) -> None:
        try:
            import levelgen.gui as gui_module
        except ImportError:
            self.skipTest("PySide6 unavailable")
        self.assertTrue(hasattr(gui_module, "main"))
        self.assertIsInstance(gui_module.PYSIDE_AVAILABLE, bool)

    def test_baseline_and_trace_hashes_are_content_derived(self) -> None:
        result = generate(REQUEST)
        self.assertTrue(result.ok)
        trace_hash = trace_content_hash(result.trace)
        self.assertEqual(len(trace_hash), 64)
        self.assertNotIn("timestamp", json.dumps(result.trace).lower())
        baselines = json.loads(
            (ROOT / "artifacts" / "baselines" / "hashes.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(
            baselines["s24_std"]["trace_hash"],
            trace_hash,
        )


if __name__ == "__main__":
    unittest.main()
