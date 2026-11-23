from __future__ import annotations

import unittest
from dataclasses import replace

from gamepad_studio.compare import compare_results
from gamepad_studio.errors import DivergenceError, SampleValidationError
from gamepad_studio.models import CurveSpec
from gamepad_studio.pipeline import Pipeline
from gamepad_studio.replay import replay_full

from tests.helpers import load_bundle


class PipelineTests(unittest.TestCase):
    def test_initial_pipeline_matches_fresh_full_replay(self) -> None:
        bundle = load_bundle("dual_stick")
        run = Pipeline(bundle).recompute()
        full = replay_full(bundle)
        self.assertTrue(compare_results(run.result, full).equal)
        self.assertEqual(run.reused_count, 0)
        self.assertGreater(run.recomputed_count, 0)

    def test_deadzone_change_invalidates_only_axis_rule_action_output_chain(self) -> None:
        bundle = load_bundle("dual_stick")
        pipeline = Pipeline(bundle)
        initial = pipeline.recompute()
        untouched_axis_cache = pipeline.cache_entry("axis:left_y")
        untouched_rule_cache = pipeline.cache_entry("rule:jump")
        device = bundle.device.with_axis("left_x", deadzone=0.2)
        run = pipeline.update(device=device)
        invalidated = {
            entry.node_id: entry
            for entry in run.invalidations
            if entry.invalidated
        }
        self.assertEqual(
            set(invalidated),
            {"axis:left_x", "rule:move_x", "action:move_x", "output"},
        )
        self.assertIn("parameter_changed:deadzone", invalidated["axis:left_x"].reasons)
        self.assertIn("w0", invalidated["axis:left_x"].window_ids)
        self.assertEqual(run.reused_count, 29)
        self.assertIs(untouched_axis_cache, pipeline.cache_entry("axis:left_y"))
        self.assertIs(untouched_rule_cache, pipeline.cache_entry("rule:jump"))
        fresh = Pipeline(replace(bundle, device=device)).recompute()
        self.assertTrue(compare_results(run.result, fresh.result).equal)
        self.assertNotEqual(initial.result.result_hash, run.result.result_hash)

    def test_binding_rule_change_reuses_all_axis_nodes(self) -> None:
        bundle = load_bundle("dual_stick")
        pipeline = Pipeline(bundle)
        pipeline.recompute()
        bindings = bundle.bindings.with_rule(
            "move_x",
            curve=CurveSpec(kind="power", exponent=2.0),
        )
        run = pipeline.update(bindings=bindings)
        invalidated = {
            entry.node_id for entry in run.invalidations if entry.invalidated
        }
        self.assertEqual(
            invalidated,
            {"rule:move_x", "action:move_x", "output"},
        )
        self.assertFalse(any(node.startswith("axis:") for node in invalidated))
        fresh = Pipeline(replace(bundle, bindings=bindings)).recompute()
        self.assertTrue(compare_results(run.result, fresh.result).equal)

    def test_timing_change_invalidates_timing_rule_action_output(self) -> None:
        bundle = load_bundle("dual_stick")
        pipeline = Pipeline(bundle)
        pipeline.recompute()
        bindings = bundle.bindings.with_rule("sprint", window_ms=100)
        run = pipeline.update(bindings=bindings)
        invalidated = [
            entry.node_id for entry in run.invalidations if entry.invalidated
        ]
        self.assertEqual(
            set(invalidated),
            {"timing:sprint", "rule:sprint", "action:sprint", "output"},
        )
        fresh = Pipeline(replace(bundle, bindings=bindings)).recompute()
        self.assertTrue(compare_results(run.result, fresh.result).equal)

    def test_manual_retry_marks_node_and_downstream_for_recompute(self) -> None:
        bundle = load_bundle("dual_stick")
        pipeline = Pipeline(bundle)
        pipeline.recompute()
        run = pipeline.update(force_node_ids=("rule:jump",))
        invalidated = {
            entry.node_id for entry in run.invalidations if entry.invalidated
        }
        self.assertEqual(invalidated, {"rule:jump", "action:jump", "output"})
        self.assertIn("manual_retry", next(
            entry.reasons
            for entry in run.invalidations
            if entry.node_id == "rule:jump"
        ))

    def test_divergence_reports_first_frame_action_and_field(self) -> None:
        bundle = load_bundle("dual_stick")
        result = replay_full(bundle)
        frame = result.frames[0]
        action_id = result.action_ids[0]
        changed_snapshot = replace(frame.actions[action_id], value=frame.actions[action_id].value + 0.5)
        changed_frame = replace(
            frame,
            actions={**frame.actions, action_id: changed_snapshot},
        )
        changed_result = replace(result, frames=(changed_frame,) + result.frames[1:])
        diff = compare_results(changed_result, result)
        self.assertFalse(diff.equal)
        self.assertEqual(diff.first_frame_index, 0)
        self.assertEqual(diff.action, action_id)
        self.assertEqual(diff.field, "value")
        with self.assertRaises(DivergenceError) as caught:
            compare_results(changed_result, result, raise_on_divergence=True)
        self.assertEqual(caught.exception.code, "E_DIVERGE")
        self.assertEqual(caught.exception.event_seq, frame.source_event_seq)

    def test_invalid_timing_update_returns_parameter_error(self) -> None:
        bundle = load_bundle("dual_stick")
        pipeline = Pipeline(bundle)
        pipeline.recompute()
        bindings = bundle.bindings.with_rule("sprint", window_ms=0)
        with self.assertRaises(SampleValidationError) as caught:
            pipeline.update(bindings=bindings)
        self.assertEqual(caught.exception.code, "E_PARA")
        self.assertIn("window_ms", caught.exception.field)


if __name__ == "__main__":
    unittest.main()

