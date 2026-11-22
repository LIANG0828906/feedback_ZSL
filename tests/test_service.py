from __future__ import annotations

import unittest

from sync_kernel.client import LoopbackClient
from sync_kernel.errors import E_SEQ
from sync_kernel.samples import build_sample_model
from sync_kernel.service import LoopbackServer


class LoopbackServiceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        (
            cls.bundle,
            cls.context,
            cls.initial,
            cls.inputs,
            cls.states,
        ) = build_sample_model(ticks=24)

    def test_versions_converge_under_injected_faults(self) -> None:
        server = LoopbackServer(frames_by_version=self.bundle.frames_by_version)
        server.start()
        clients = []
        try:
            for version in (1, 2, 3):
                client = LoopbackClient(
                    host=server.host,
                    port=server.port,
                    version=version,
                    context=self.context,
                    initial_state=self.initial,
                    inputs_by_tick=self.inputs,
                    timeout=8.0,
                )
                client.connect()
                clients.append(client)
            for client in clients:
                self.assertTrue(
                    client.wait(8.0),
                    msg=f"client v{client.predictor.version} timed out",
                )
                self.assertEqual(
                    client.confirmed_state.snapshot_hash,
                    self.states[-1].snapshot_hash,
                )
                self.assertIn(
                    E_SEQ,
                    {issue.code for issue in client.predictor.issues},
                )
                self.assertGreaterEqual(len(client.echoes), 1)
            self.assertGreaterEqual(server.stats.dropped, 3)
            self.assertGreaterEqual(server.stats.duplicated, 3)
            self.assertGreaterEqual(server.stats.reordered, 3)
            self.assertGreater(server.stats.reports_received, 0)
        finally:
            for client in clients:
                client.close()
            server.stop()


if __name__ == "__main__":
    unittest.main()

