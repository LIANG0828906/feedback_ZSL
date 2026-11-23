from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from gamepad_studio.service import StudioSession

from tests.helpers import load_bundle


class ServiceSnapshotTests(unittest.TestCase):
    def test_invalid_in_session_snapshot_reload_forces_full_replay(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="padweave-service-")
        snapshot_path = Path(temporary.name) / "state.json"
        try:
            session = StudioSession(
                bundle=load_bundle("dual_stick"),
                snapshot_path=snapshot_path,
                restore_snapshot=False,
            )
            initial_hash = session.last_run.result.result_hash
            initial_nodes = len(session.pipeline.order)
            session.save_snapshot()
            payload = json.loads(snapshot_path.read_text(encoding="utf-8"))
            payload["metadata"]["input_hash"] = "deliberate-mismatch"
            snapshot_path.write_text(json.dumps(payload), encoding="utf-8")
            load = session.reload_snapshot()
            self.assertFalse(load.valid)
            self.assertEqual(load.reason, "version_mismatch")
            self.assertEqual(session.last_run.recomputed_count, initial_nodes)
            self.assertEqual(session.last_run.reused_count, 0)
            self.assertEqual(session.last_run.result.result_hash, initial_hash)
            self.assertEqual(len(session.pipeline.caches), initial_nodes)
            session.close()
        finally:
            temporary.cleanup()


if __name__ == "__main__":
    unittest.main()
