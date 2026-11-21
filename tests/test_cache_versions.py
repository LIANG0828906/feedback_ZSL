import unittest

from hdrworkbench import color
from hdrworkbench.cache import FrameCache
from hdrworkbench.errors import E_VERSION_STALE, VersionStaleError
from hdrworkbench.generator import SampleSpec, generate_frames
from hdrworkbench.hdrformat import (
    CS_DISPLAY_P3,
    CS_REC2020,
    CS_REC709,
    FrameSequence,
    Header,
    TF_PQ,
)
from hdrworkbench.pipeline import ConversionConfig, convert_sequence
from hdrworkbench.versions import VersionStore


def _sequence():
    spec = SampleSpec(8, 6, 3, 10, CS_REC2020, TF_PQ, 1000, seed=5)
    header = Header(
        spec.width,
        spec.height,
        spec.frame_count,
        spec.bit_depth,
        spec.colorspace,
        spec.transfer,
        spec.peak_nits,
    )
    return FrameSequence(header=header, raw_frames=generate_frames(spec))


class CacheTests(unittest.TestCase):
    def setUp(self):
        self.seq = _sequence()
        self.hashes = [str(i) for i in range(3)]
        self.cfg = ConversionConfig(
            target_colorspace=CS_REC709,
            target_transfer=TF_PQ,
            target_bit_depth=10,
            target_peak_nits=1000,
            tone_curve=color.TONE_REINHARD,
            mode="fixed",
            frac_bits=12,
        )

    def test_same_factors_full_reuse(self):
        cache = FrameCache()
        results = convert_sequence(self.seq, self.cfg)
        for index, result in enumerate(results):
            cache.put(index, self.hashes[index], self.cfg, result)
        plan = cache.plan(self.hashes, self.cfg, self.cfg)
        self.assertEqual(plan.reused_frames, [0, 1, 2])
        self.assertEqual(plan.affected_frames, [])

    def test_peak_change_marks_all_affected_with_reason(self):
        cache = FrameCache()
        results = convert_sequence(self.seq, self.cfg)
        for index, result in enumerate(results):
            cache.put(index, self.hashes[index], self.cfg, result)
        new_cfg = ConversionConfig(
            **{**self.cfg.factor_dict(), "target_peak_nits": 2000}
        )
        plan = cache.plan(self.hashes, self.cfg, new_cfg)
        self.assertEqual(plan.affected_frames, [0, 1, 2])
        self.assertIn("target_peak_nits", plan.changed_factors)
        self.assertTrue(all(plan.conservative_reasons.values()))

    def test_missing_source_hash_conservative(self):
        cache = FrameCache()
        results = convert_sequence(self.seq, self.cfg)
        cache.put(0, self.hashes[0], self.cfg, results[0])
        plan = cache.plan([""], self.cfg, self.cfg)
        self.assertEqual(plan.affected_frames, [0])
        self.assertEqual(
            plan.conservative_reasons[0], "missing_source_hash"
        )

    def test_frac_bits_is_affected_factor(self):
        cache = FrameCache()
        results = convert_sequence(self.seq, self.cfg)
        for index, result in enumerate(results):
            cache.put(index, self.hashes[index], self.cfg, result)
        new_cfg = ConversionConfig(
            **{**self.cfg.factor_dict(), "frac_bits": 20}
        )
        plan = cache.plan(self.hashes, self.cfg, new_cfg)
        self.assertIn("frac_bits", plan.changed_factors)


class VersionStoreTests(unittest.TestCase):
    def setUp(self):
        self.seq = _sequence()

    def _publish(self, store, cfg, version_id):
        from hdrworkbench.experiment import payload_from_result
        from hdrworkbench.versions import VersionBuilder

        results = convert_sequence(self.seq, cfg)
        builder = VersionBuilder(
            version_id,
            cfg,
            self.seq.header,
            list(range(3)),
            [],
        )
        for result in results:
            result.bound_summary = {"max_bound_codes": 1.0}
            builder.add_frame(payload_from_result(result))
        return store.publish(builder.publish())

    def test_immutable_versions_and_query_hash(self):
        store = VersionStore()
        cfg = ConversionConfig(
            target_colorspace=CS_REC709,
            target_transfer=TF_PQ,
            target_bit_depth=10,
            target_peak_nits=1000,
            tone_curve=color.TONE_REINHARD,
            mode="float",
        )
        h1 = self._publish(store, cfg, 1)
        cfg2 = ConversionConfig(
            **{**cfg.factor_dict(), "target_colorspace": CS_DISPLAY_P3}
        )
        h2 = self._publish(store, cfg2, 2)
        self.assertNotEqual(h1, h2)
        old = store.get(h1)
        current = store.current()
        self.assertEqual(current.version_hash, h2)
        self.assertEqual(old.version_id, 1)
        query = store.query_frames([0], version_hash=h1)
        self.assertEqual(query["version_hash"], h1)

    def test_tampered_version_raises_stale_and_returns_no_frames(self):
        store = VersionStore()
        cfg = ConversionConfig(
            target_colorspace=CS_REC709,
            target_transfer=TF_PQ,
            target_bit_depth=10,
            target_peak_nits=1000,
            tone_curve=color.TONE_REINHARD,
            mode="float",
        )
        h1 = self._publish(store, cfg, 1)
        store.tamper(h1)
        with self.assertRaises(VersionStaleError) as ctx:
            store.query_frames([0], version_hash=h1)
        self.assertEqual(ctx.exception.code, E_VERSION_STALE)


if __name__ == "__main__":
    unittest.main()

