from __future__ import annotations

import unittest

from scworkbench.anomalies import default_presets
from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.loader import parse_session

from tests.helpers import code_set, sample_data


class AnomalyChainTests(unittest.TestCase):
    EXPECTED = {
        "clean": {"E_STATE"},
        "loss": {"E_LOSS", "E_STATE"},
        "reorder": {"E_ORDER", "E_STATE"},
        "capability": {"E_CAP", "E_STATE"},
        "corrupt": {"E_CORRUPT", "E_STATE"},
        "mixed": {
            "E_CAP",
            "E_CORRUPT",
            "E_DUP",
            "E_LOSS",
            "E_ORDER",
            "E_SESSION",
            "E_STATE",
        },
    }

    def test_each_anomaly_chain_returns_expected_code(self) -> None:
        sample = parse_session(sample_data())
        presets = default_presets()
        for name, expected in self.EXPECTED.items():
            with self.subTest(name=name):
                with SessionEngine(
                    sample,
                    config=EngineConfig(
                        checkpoint_every=0, anomaly=presets[name]
                    ),
                ) as engine:
                    engine.run_to_end()
                    result = engine.to_run_result(name)
                codes = code_set(result)
                self.assertTrue(
                    expected.issubset(codes),
                    f"{name}: expected {expected}, got {codes}",
                )
                self.assertEqual(result.stats.frames_committed, 40)
                self.assertEqual(
                    result.stats.frames_sent,
                    result.stats.frames_reassembled,
                )

    def test_persistent_loss_creates_missing_frame_marker(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(
            sample,
            config=EngineConfig(
                checkpoint_every=0, anomaly=default_presets()["loss"]
            ),
        ) as engine:
            engine.run_to_end()
            result = engine.to_run_result("loss")
        missing = [frame for frame in result.frames if frame.status == "missing"]
        self.assertEqual([frame.frame_seq for frame in missing], [18])
        self.assertEqual(missing[0].missing_fragments, [1])
        self.assertGreaterEqual(missing[0].retransmissions, 2)

    def test_corrupt_fragment_retransmits_and_recovers(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(
            sample,
            config=EngineConfig(
                checkpoint_every=0, anomaly=default_presets()["corrupt"]
            ),
        ) as engine:
            engine.run_to_end()
            result = engine.to_run_result("corrupt")
        frame = next(item for item in result.frames if item.frame_seq == 12)
        self.assertEqual(frame.status, "committed")
        self.assertIn("E_CORRUPT", frame.codes)
        self.assertGreaterEqual(frame.retransmissions, 1)
        self.assertGreaterEqual(result.stats.retransmission_recovered, 1)


if __name__ == "__main__":
    unittest.main()
