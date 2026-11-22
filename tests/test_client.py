from __future__ import annotations

import unittest

from sync_kernel.client import PredictiveClient
from sync_kernel.errors import E_SEQ
from sync_kernel.models import first_authority_difference
from sync_kernel.samples import build_sample_model


class PredictiveClientTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        (
            cls.bundle,
            cls.context,
            cls.initial,
            cls.inputs,
            cls.states,
        ) = build_sample_model(ticks=16)
        cls.local_inputs = {
            tick: [
                intent
                for intent in intents
                if intent.get("player_id") == "p1"
            ]
            for tick, intents in cls.inputs.items()
        }

    def make_client(self) -> PredictiveClient:
        return PredictiveClient(
            version=3,
            context=self.context,
            initial_state=self.initial,
            inputs_by_tick=self.local_inputs,
        )

    def test_prediction_rollback_and_replay_matches_authority(self) -> None:
        client = self.make_client()
        client.predict_to(10)
        for tick in range(0, 11):
            result = client.feed(self.bundle.frames_by_version[3][tick])
            self.assertTrue(result.applied, msg=f"tick={tick}")
            self.assertIsNone(
                first_authority_difference(
                    client.confirmed_state,
                    self.states[tick],
                )
            )
        self.assertGreater(len(client.corrections), 0)
        self.assertTrue(any(correction.delta != 0 for correction in client.corrections))
        self.assertEqual(
            client.confirmed_state.snapshot_hash,
            self.states[10].snapshot_hash,
        )

    def test_gap_and_reverse_return_e_seq_without_state_damage(self) -> None:
        client = self.make_client()
        client.predict_to(5)
        self.assertTrue(client.feed(self.bundle.frames_by_version[3][0]).applied)
        confirmed_hash = client.confirmed_state.snapshot_hash
        gap = client.feed(self.bundle.frames_by_version[3][2])
        self.assertFalse(gap.applied)
        self.assertIn(E_SEQ, [issue.code for issue in gap.issues])
        self.assertEqual(client.confirmed_state.snapshot_hash, confirmed_hash)

        fill = client.feed(self.bundle.frames_by_version[3][1])
        self.assertTrue(fill.applied)
        self.assertEqual(client.confirmed_sequence, 2)
        filled_hash = client.confirmed_state.snapshot_hash
        reverse = client.feed(self.bundle.frames_by_version[3][2])
        self.assertFalse(reverse.applied)
        self.assertIn(E_SEQ, [issue.code for issue in reverse.issues])
        self.assertEqual(client.confirmed_state.snapshot_hash, filled_hash)

    def test_all_versions_rollback_to_expected_hashes(self) -> None:
        for version in (1, 2, 3):
            client = PredictiveClient(
                version=version,
                context=self.context,
                initial_state=self.initial,
                inputs_by_tick=self.local_inputs,
            )
            client.predict_to(8)
            for tick in range(0, 9):
                result = client.feed(self.bundle.frames_by_version[version][tick])
                self.assertTrue(result.applied)
                self.assertEqual(
                    client.confirmed_state.snapshot_hash,
                    self.states[tick].snapshot_hash,
                )


if __name__ == "__main__":
    unittest.main()

