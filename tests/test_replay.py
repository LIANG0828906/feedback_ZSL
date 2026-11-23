from __future__ import annotations

import unittest
from concurrent.futures import ThreadPoolExecutor

from gamepad_studio.curves import normalize_axis
from gamepad_studio.models import AxisSpec, BindingRule, CurveSpec, InputEvent
from gamepad_studio.replay import check_truth, replay_full

from tests.helpers import button_event, load_bundle, synthetic_bundle


class ReplayTests(unittest.TestCase):
    def test_axis_deadzone_inversion_and_curve_order(self) -> None:
        axis = AxisSpec(
            id="left_x",
            kind="stick",
            minimum=-1.0,
            maximum=1.0,
            deadzone=0.2,
            inverted=False,
            curve=CurveSpec(kind="power", exponent=2.0),
        )
        oriented, normalized = normalize_axis(axis, 0.6)
        self.assertAlmostEqual(oriented, 0.5)
        self.assertAlmostEqual(normalized, 0.25)
        inverted = AxisSpec(
            id="left_y",
            kind="stick",
            minimum=-1.0,
            maximum=1.0,
            deadzone=0.2,
            inverted=True,
        )
        oriented, normalized = normalize_axis(inverted, 0.6)
        self.assertAlmostEqual(oriented, -0.5)
        self.assertAlmostEqual(normalized, -0.5)
        _, dead = normalize_axis(axis, 0.2)
        self.assertEqual(dead, 0.0)

    def test_truth_manifest_all_devices(self) -> None:
        for device_id in ("dual_stick", "single_stick", "hybrid_trigger_gyro"):
            result = replay_full(load_bundle(device_id))
            report = check_truth(load_bundle(device_id), result)
            self.assertTrue(report.passed, [failure.to_dict() for failure in report.failures])

    def test_long_press_boundary_is_inclusive_and_timer_precedes_release(self) -> None:
        rule = BindingRule(
            id="hold",
            action="charge",
            kind="long_press",
            buttons=("a",),
            hold_ms=100,
        )
        bundle = synthetic_bundle(
            events=[
                button_event(0, 0, "a", True),
                button_event(1, 100, "a", False),
            ],
            rules=[rule],
            buttons=("a",),
        )
        result = replay_full(bundle)
        trigger_frames = [
            frame
            for frame in result.frames
            if frame.actions["charge"].triggered_now
        ]
        self.assertEqual(len(trigger_frames), 1)
        self.assertEqual(trigger_frames[0].t_ms, 100)
        self.assertEqual(trigger_frames[0].actions["charge"].trigger_seq, 0)
        self.assertFalse(result.frames[-1].actions["charge"].pressed)

    def test_long_press_release_before_threshold_does_not_trigger(self) -> None:
        rule = BindingRule(
            id="hold",
            action="charge",
            kind="long_press",
            buttons=("a",),
            hold_ms=100,
        )
        bundle = synthetic_bundle(
            events=[
                button_event(0, 0, "a", True),
                button_event(1, 99, "a", False),
            ],
            rules=[rule],
            buttons=("a",),
        )
        result = replay_full(bundle)
        self.assertEqual(result.frames[-1].actions["charge"].trigger_count, 0)
        self.assertEqual(
            result.frames[-1].actions["charge"].last_reason,
            "long_released_early",
        )

    def test_combo_window_boundary_is_inclusive(self) -> None:
        rule = BindingRule(
            id="combo",
            action="sprint",
            kind="combo",
            buttons=("a", "b"),
            window_ms=100,
        )
        on_boundary = synthetic_bundle(
            events=[
                button_event(0, 0, "a", True),
                button_event(1, 100, "b", True),
                button_event(2, 120, "a", False),
            ],
            rules=[rule],
        )
        frames = replay_full(on_boundary).frames
        triggers = [frame for frame in frames if frame.actions["sprint"].triggered_now]
        self.assertEqual(len(triggers), 1)
        self.assertEqual(triggers[0].t_ms, 100)
        outside = synthetic_bundle(
            events=[
                button_event(0, 0, "a", True),
                button_event(1, 101, "b", True),
                button_event(2, 120, "a", False),
            ],
            rules=[rule],
        )
        result = replay_full(outside)
        self.assertEqual(result.frames[-1].actions["sprint"].trigger_count, 0)
        self.assertTrue(
            any(
                frame.actions["sprint"].last_reason == "combo_window_expired"
                for frame in result.frames
            )
        )

    def test_double_press_window_boundary_is_inclusive(self) -> None:
        rule = BindingRule(
            id="double",
            action="dash",
            kind="double_press",
            buttons=("b",),
            window_ms=200,
        )
        boundary = synthetic_bundle(
            events=[
                button_event(0, 0, "b", True),
                button_event(1, 50, "b", False),
                button_event(2, 150, "b", True),
                button_event(3, 200, "b", False),
            ],
            rules=[rule],
            buttons=("a", "b"),
        )
        result = replay_full(boundary)
        triggers = [frame for frame in result.frames if frame.actions["dash"].triggered_now]
        self.assertEqual(len(triggers), 1)
        self.assertEqual(triggers[0].t_ms, 200)
        self.assertEqual(triggers[0].actions["dash"].trigger_seq, 3)

    def test_auto_repeat_retriggers_button_action(self) -> None:
        result = replay_full(load_bundle("dual_stick"))
        jump = result.frames[-1].actions["jump"]
        self.assertEqual(jump.trigger_count, 3)
        self.assertEqual(jump.repeat_count, 2)
        repeats = [
            frame
            for frame in result.frames
            if frame.actions["jump"].last_reason == "button_auto_repeat"
        ]
        self.assertEqual([frame.t_ms for frame in repeats], [200, 240])

    def test_replay_is_independent_of_thread_and_completion_order(self) -> None:
        bundle = load_bundle("dual_stick")
        with ThreadPoolExecutor(max_workers=4) as executor:
            hashes = list(executor.map(lambda _: replay_full(bundle).result_hash, range(8)))
        self.assertEqual(len(set(hashes)), 1)


if __name__ == "__main__":
    unittest.main()

