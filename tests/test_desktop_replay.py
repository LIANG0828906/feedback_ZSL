from __future__ import annotations

import unittest

from scworkbench.desktop import DesktopModel
from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.loader import parse_session
from scworkbench.models import InputEvent
from scworkbench.negotiation import negotiate

from tests.helpers import sample_data


class DesktopTests(unittest.TestCase):
    def setUp(self) -> None:
        self.sample = parse_session(sample_data())
        self.negotiation = negotiate(self.sample)
        self.desktop = DesktopModel(
            declared_width=640,
            declared_height=360,
            width=320,
            height=180,
        )
        self.desktop.observe_scene(self.sample.frames[0].scene, 1)

    def test_coordinate_scaling_and_clipping_trace(self) -> None:
        point, trace = self.desktop.map_point(720, -7)
        self.assertEqual(point, (319, 0))
        self.assertEqual(len(trace), 2)
        self.assertEqual(trace[0]["reason"], "above_max")
        self.assertEqual(trace[1]["reason"], "below_min")
        self.assertEqual(trace[0]["original"], 720)
        self.assertEqual(trace[0]["clipped"], 319)

    def test_drag_moves_window_and_records_both_clip_fields(self) -> None:
        event = InputEvent(
            seq=1,
            timestamp_ms=10,
            kind="drag",
            x=80,
            y=100,
            to_x=720,
            to_y=410,
        )
        mapped, trace = self.desktop.apply(event)
        self.assertEqual(mapped[0], 40)
        self.assertEqual(mapped[1], 50)
        self.assertEqual(mapped[2], 319)
        self.assertEqual(mapped[3], 179)
        self.assertTrue(any(item["field"] == "to_x" for item in trace))
        self.assertTrue(any(item["field"] == "to_y" for item in trace))
        self.assertEqual(self.desktop.window_offsets[1], [279, 129])
        self.assertEqual(self.desktop.drag_count, 1)

    def test_duplicate_input_is_e_dup_and_state_is_idempotent(self) -> None:
        clean = parse_session(sample_data())
        duplicate_data = sample_data()
        with SessionEngine(
            clean, EngineConfig(checkpoint_every=0)
        ) as clean_engine:
            clean_engine.run_to_end()
            clean_result = clean_engine.to_run_result("clean")
        duplicate = parse_session(duplicate_data)
        anomaly = EngineConfig(checkpoint_every=0).anomaly
        anomaly.duplicate_inputs = [7]
        config = EngineConfig(checkpoint_every=0, anomaly=anomaly)
        with SessionEngine(duplicate, config) as duplicate_engine:
            duplicate_engine.run_to_end()
            duplicate_result = duplicate_engine.to_run_result("duplicate")
        self.assertEqual(duplicate_result.stats.inputs_duplicate, 1)
        input_seven = next(
            item for item in duplicate_result.inputs if item.input_seq == 7
        )
        self.assertIn("E_DUP", input_seven.codes)
        self.assertEqual(
            duplicate_result.desktop_state, clean_result.desktop_state
        )
        self.assertEqual(
            [item.frame_sha256 for item in clean_result.frames],
            [item.frame_sha256 for item in duplicate_result.frames],
        )
        self.assertEqual(
            [item.desktop_digest for item in clean_result.inputs],
            [item.desktop_digest for item in duplicate_result.inputs],
        )

    def test_out_of_bounds_sample_drag_records_trace(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(sample, EngineConfig(checkpoint_every=0)) as engine:
            engine.run_to_end()
            result = engine.to_run_result("clip")
        clipped = next(item for item in result.inputs if item.input_seq == 22)
        self.assertTrue(clipped.clipped)
        self.assertEqual(
            {item["field"] for item in clipped.clip_trace},
            {"to_x", "to_y"},
        )
        self.assertEqual(clipped.mapped_coords[-2:], [639, 359])


if __name__ == "__main__":
    unittest.main()
