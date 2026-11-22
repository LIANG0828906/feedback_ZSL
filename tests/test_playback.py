import numpy as np

from scoreworkbench.errors import ParameterError
from scoreworkbench.model import ExpressionEvent
from scoreworkbench.playback import FakeClock, PlaybackEngine
from scoreworkbench.versioning import load_score

from .base import SampleBackedTestCase


class PlaybackTests(SampleBackedTestCase):
    def setUp(self):
        self.project = load_score(self.v3)
        self.score = self.project.score
        self.clock = FakeClock()
        self.engine = PlaybackEngine(self.score, self.clock)

    def test_play_pause_and_seek(self):
        self.engine.play(start_beat=0.0)
        self.assertTrue(self.engine.is_playing)
        self.clock.advance(1.0)
        beat_after_one_second = self.engine.current_beat()
        expected = self.score.seconds_to_beat(1.0)
        self.assertAlmostEqual(beat_after_one_second, expected, places=6)
        paused_beat = self.engine.pause()
        self.assertFalse(self.engine.is_playing)
        self.clock.advance(5.0)
        self.assertAlmostEqual(self.engine.current_beat(), paused_beat)

    def test_seek_to_arbitrary_beat(self):
        target = self.engine.total_beats / 2
        self.engine.seek(target)
        self.engine.play()
        self.assertAlmostEqual(self.engine.current_beat(), target)

    def test_out_of_range_beat_raises_e_para(self):
        with self.assertRaises(ParameterError) as catcher:
            self.engine.play(start_beat=self.engine.total_beats + 4)
        self.assertIn("valid_range", catcher.exception.details)

    def test_invalid_velocity_event_raises_e_para(self):
        velocity_event = next(
            event for event in self.score.events if event.kind == "velocity"
        )
        velocity_event.value = 2.5
        with self.assertRaises(ParameterError) as catcher:
            self.engine.validate_event_ranges()
        self.assertEqual(
            catcher.exception.details["source_event_id"],
            velocity_event.event_id,
        )

    def test_velocity_curve_is_numpy_array(self):
        curve = self.engine.velocity_curve(128)
        self.assertIsInstance(curve, np.ndarray)
        self.assertEqual(curve.shape, (128,))
        self.assertTrue(np.all((curve >= 0) & (curve <= 1)))

    def test_pedal_and_legato_are_visible(self):
        pedal = self.engine.pedal_curve(64)
        self.assertGreater(float(pedal.max()), 0.0)
        spans = self.engine.legato_spans()
        self.assertTrue(spans)
        for start, end, event_id in spans:
            self.assertGreater(end, start)
            self.assertTrue(event_id.startswith("e"))

    def test_frame_reports_active_expression_state(self):
        self.engine.play(0.0)
        frame = self.engine.frame()
        self.assertGreaterEqual(frame.velocity, 0.0)
        self.assertIsInstance(frame.active_event_ids, list)

    def test_absolute_seconds_match_across_versions(self):
        projects = {
            project.source_version: project
            for project in __import__(
                "scoreworkbench.versioning", fromlist=["load_many"]
            ).load_many([self.v1, self.v2, self.v3])
        }
        reference = projects["3.0"].score
        reference_seconds = {
            event.event_id: reference.beat_to_seconds(
                reference.event_absolute_beat(event)
            )
            for event in reference.events
        }
        for version in ("1.0", "2.0"):
            score = projects[version].score
            for event in score.events:
                seconds = score.beat_to_seconds(
                    score.event_absolute_beat(event)
                )
                self.assertAlmostEqual(
                    seconds, reference_seconds[event.event_id], places=5
                )

    def test_stop_resets_cursor(self):
        self.engine.play(2.0)
        self.clock.advance(1.0)
        self.engine.stop()
        self.assertEqual(self.engine.current_beat(), 0.0)
        self.assertFalse(self.engine.is_playing)
