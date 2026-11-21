from __future__ import annotations

import unittest
from dataclasses import replace

from tileforge.color import ColorPipeline, convert_tile, tile_fingerprint
from tileforge.samplegen import SAMPLE_SPECS, generate_sequence, sample_config


class ColorChainTests(unittest.TestCase):
    def setUp(self) -> None:
        self.sequence = generate_sequence(SAMPLE_SPECS[0], 72)
        self.config = sample_config(SAMPLE_SPECS[0])

    def test_conversion_is_repeatable_and_profile_invariant(self) -> None:
        block = self.sequence.ordered_blocks()[0]
        first = convert_tile(self.sequence, block, self.config, profile="full")
        second = convert_tile(self.sequence, block, self.config, profile="full")
        streamed = convert_tile(self.sequence, block, self.config, profile="streaming")
        self.assertEqual(first.data, second.data)
        self.assertEqual(first.content_hash, second.content_hash)
        self.assertEqual(first.data, streamed.data)
        self.assertEqual(first.content_hash, streamed.content_hash)
        self.assertEqual(first.max_code_delta, streamed.max_code_delta)
        self.assertNotEqual(first.profile, streamed.profile)

    def test_rec709_sdr_identity_round_trip(self) -> None:
        config = replace(
            self.config,
            source=replace(self.config.source, primaries=self.config.source.primaries, transfer=self.config.source.transfer),
            target=replace(
                self.config.target,
                primaries=self.config.source.primaries,
                transfer=self.config.source.transfer,
                peak_nits=self.config.source.peak_nits,
            ),
            tone_map_curve="none",
        )
        pipeline = ColorPipeline.from_config(config)
        for code in (0.0, 0.02, 0.18, 0.5, 1.0):
            encoded = pipeline.convert_pixel((int(round(code * 255)),) * 3, 255, 0.0, "none")
            actual = [int(round(value * 255)) for value in encoded]
            for value in actual:
                self.assertLessEqual(abs(value - int(round(code * 255))), 1)

    def test_only_changed_tile_fingerprint_changes(self) -> None:
        blocks = self.sequence.ordered_blocks()
        before = {block.tile_id: tile_fingerprint(self.sequence, block, self.config) for block in blocks}
        changed = replace(self.config, block_exposure_stops={3: 0.4})
        after = {block.tile_id: tile_fingerprint(self.sequence, block, changed) for block in blocks}
        changed_ids = [tile_id for tile_id in before if before[tile_id] != after[tile_id]]
        self.assertEqual(changed_ids, [3])

    def test_conversion_order_does_not_matter(self) -> None:
        blocks = self.sequence.ordered_blocks()
        forward = {block.tile_id: convert_tile(self.sequence, block, self.config).content_hash for block in blocks}
        reverse = {
            block.tile_id: convert_tile(self.sequence, block, self.config).content_hash
            for block in reversed(blocks)
        }
        self.assertEqual(forward, reverse)


if __name__ == "__main__":
    unittest.main()
