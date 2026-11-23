from __future__ import annotations

import unittest

from gamepad_studio.errors import MappingError
from gamepad_studio.session import HotSwitchSession, plan_hot_switch

from tests.helpers import load_bundle


class HotSwitchTests(unittest.TestCase):
    def setUp(self) -> None:
        self.source = load_bundle("dual_stick")
        self.target = load_bundle("hybrid_trigger_gyro")

    def test_incompatible_source_bindings_return_map_with_axis_identity(self) -> None:
        analysis = plan_hot_switch(
            self.source,
            self.target,
            target_bindings=self.source.bindings,
        )
        self.assertGreater(len(analysis.conflicts), 0)
        with self.assertRaises(MappingError) as caught:
            HotSwitchSession.raise_for_conflicts(analysis)
        exc = caught.exception
        self.assertEqual(exc.code, "E_MAP")
        self.assertEqual(exc.context["source_device"], "dual_stick")
        self.assertEqual(exc.context["target_device"], "hybrid_trigger_gyro")
        self.assertTrue(exc.context["axis_id"])

    def test_successful_switch_preserves_prefix_and_matches_direct_target(self) -> None:
        session = HotSwitchSession(
            self.source,
            self.target,
            switch_t_ms=2000,
        )
        result = session.result
        self.assertTrue(result.verify_prefix())
        self.assertTrue(result.direct_match)
        self.assertTrue(session.verify_direct_target()["equal"])
        self.assertTrue(all(frame.t_ms <= 2000 for frame in result.prefix_frames))
        self.assertTrue(all(frame.t_ms > 2000 for frame in result.suffix_frames))
        unmapped_ids = {item.input_id for item in result.analysis.unmapped}
        self.assertIn("left_x", unmapped_ids)
        self.assertIn("gyro_y", unmapped_ids)
        self.assertGreater(len(result.analysis.migration_notes), 0)

    def test_target_update_does_not_rewrite_committed_prefix(self) -> None:
        session = HotSwitchSession(
            self.source,
            self.target,
            switch_t_ms=2000,
        )
        original_hash = session.result.prefix_hash
        session.update_target(
            device=self.target.device.with_axis("stick_x", deadzone=0.23)
        )
        self.assertEqual(session.result.prefix_hash, original_hash)
        self.assertTrue(session.result.verify_prefix())


if __name__ == "__main__":
    unittest.main()

