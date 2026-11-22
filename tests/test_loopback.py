from __future__ import annotations

import unittest

from scworkbench.compare import compare_runs
from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.loader import parse_session
from scworkbench.loopback import LoopbackRelayServer, LoopbackUdpClient

from tests.helpers import sample_data


class LoopbackTests(unittest.TestCase):
    def test_ping_media_and_input_echo_over_local_udp(self) -> None:
        sample = parse_session(sample_data())
        with LoopbackRelayServer() as server:
            self.assertEqual(server.host, "127.0.0.1")
            client = LoopbackUdpClient(port=server.port)
            try:
                self.assertTrue(client.ping())
                with SessionEngine(
                    sample,
                    config=EngineConfig(checkpoint_every=0),
                    transport=client,
                ) as loop_engine:
                    loop_engine.run_until_committed(3)
                    loop_result = loop_engine.to_run_result("loopback")
            finally:
                client.close()
            stats = server.stats()
        with SessionEngine(
            sample, config=EngineConfig(checkpoint_every=0)
        ) as direct_engine:
            direct_engine.run_until_committed(3)
            direct_result = direct_engine.to_run_result("direct")
        report = compare_runs(loop_result, direct_result)
        self.assertTrue(report.ok, report.to_dict())
        self.assertGreater(stats["media_received"], 0)
        self.assertGreater(stats["input_received"], 0)
        self.assertEqual(stats["invalid_requests"], 0)
        self.assertIn(sample.session_id, stats["sessions"])


if __name__ == "__main__":
    unittest.main()
