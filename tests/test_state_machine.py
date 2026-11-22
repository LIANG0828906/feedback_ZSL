from __future__ import annotations

import unittest

from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.errors import StateError
from scworkbench.loader import parse_session

from tests.helpers import sample_data


class SessionStateTests(unittest.TestCase):
    def test_paused_probe_rejected_and_resume_preserves_progress(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(
            sample, EngineConfig(checkpoint_every=0)
        ) as engine:
            engine.run_until_committed(13)
            self.assertEqual(engine.stats.frames_committed, 13)
            engine.step()
            self.assertEqual(engine.phase.value, "paused")
            engine.step()
            probe = engine.to_run_result("state").inputs[-1]
            self.assertEqual(probe.input_seq, 13)
            self.assertEqual(probe.code, "E_STATE")
            self.assertEqual(engine.stats.frames_committed, 13)
            self.assertEqual(engine.stats.inputs_replayed, 12)
            engine.step()
            self.assertEqual(engine.phase.value, "mirroring")
            engine.step()
            result = engine.to_run_result("state")
            self.assertEqual(result.frames[-1].frame_seq, 14)
            self.assertEqual(engine.stats.frames_committed, 14)
            self.assertEqual(engine.stats.inputs_replayed, 12)

    def test_frame_and_input_arriving_paused_return_e_state(self) -> None:
        data = sample_data()
        data["phase_script"] = [
            {"timestamp_ms": 0, "action": "pause"},
            {"timestamp_ms": 120, "action": "resume"},
            {"timestamp_ms": 3500, "action": "disconnect"},
        ]
        sample = parse_session(data)
        with SessionEngine(
            sample, EngineConfig(checkpoint_every=0)
        ) as engine:
            engine.run_to_end()
            result = engine.to_run_result("pause")
        rejected_frames = [f for f in result.frames if f.status == "rejected"]
        self.assertEqual(len(rejected_frames), 2)
        self.assertTrue(all("E_STATE" in frame.codes for frame in rejected_frames))
        self.assertEqual(result.stats.frames_committed, 38)
        self.assertEqual(result.frames[-1].frame_seq, 40)
        self.assertEqual(result.stats.inputs_rejected, 1)
        self.assertEqual(
            next(frame.frame_seq for frame in result.frames if frame.committed),
            3,
        )

    def test_illegal_timeline_transition_keeps_counts(self) -> None:
        data = sample_data()
        data["phase_script"][0] = {"timestamp_ms": 0, "action": "resume"}
        sample = parse_session(data)
        with SessionEngine(
            sample, EngineConfig(checkpoint_every=0)
        ) as engine:
            engine.step()
            event = engine.to_run_result("state").phase_events[-1]
            self.assertFalse(event.ok)
            self.assertEqual(event.code, "E_STATE")
            self.assertEqual(engine.phase.value, "mirroring")
            self.assertEqual(engine.stats.frames_committed, 0)
            self.assertEqual(engine.stats.inputs_replayed, 0)
            engine.step()
            self.assertEqual(engine.stats.frames_committed, 1)

    def test_public_illegal_pause_raises_e_state(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(sample, EngineConfig(checkpoint_every=0)) as engine:
            with self.assertRaises(StateError) as raised:
                engine.pause()
            self.assertEqual(raised.exception.code, "E_STATE")
            self.assertEqual(engine.phase.value, "negotiating")
            self.assertEqual(engine.stats.frames_committed, 0)

    def test_full_phase_trace(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(sample, EngineConfig(checkpoint_every=0)) as engine:
            engine.run_to_end()
            phases = [
                (event.action, event.to_phase)
                for event in engine.to_run_result("phase").phase_events
            ]
        self.assertEqual(phases[0], ("establish", "mirroring"))
        self.assertIn(("pause", "paused"), phases)
        self.assertIn(("resume", "mirroring"), phases)
        self.assertIn(("reconnect", "reconnecting"), phases)
        self.assertIn(("reconnect_complete", "mirroring"), phases)
        self.assertEqual(phases[-1], ("disconnect", "disconnected"))


if __name__ == "__main__":
    unittest.main()
