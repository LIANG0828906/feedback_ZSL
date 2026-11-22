from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.loader import parse_session
from scworkbench.replaylog import ReplayLogWriter, inspect_replay_log, write_run_log

from tests.helpers import sample_data


class ReplayLogTests(unittest.TestCase):
    def test_complete_log_has_footer_and_partial_does_not(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(sample, EngineConfig(checkpoint_every=0)) as engine:
            engine.run_to_end()
            result = engine.to_run_result("log")
        with tempfile.TemporaryDirectory() as temp:
            complete_path = Path(temp) / "complete.ndjson"
            write_run_log(result, complete_path)
            complete = inspect_replay_log(complete_path)
            self.assertTrue(complete["complete"])
            self.assertEqual(complete["status"], "complete")

            partial_path = Path(temp) / "partial.ndjson"
            with ReplayLogWriter(partial_path, {"session_id": sample.session_id}) as writer:
                writer.append({"type": "phase_transition", "phase": "mirroring"})
            partial = inspect_replay_log(partial_path)
            self.assertFalse(partial["complete"])
            self.assertEqual(partial["status"], "partial")
            self.assertEqual(partial["last_type"], "event")


if __name__ == "__main__":
    unittest.main()
