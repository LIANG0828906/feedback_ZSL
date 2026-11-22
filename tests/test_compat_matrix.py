import unittest

from afpipe.compat import (
    build_legacy_and_upgrade,
    compatibility_matrix,
    cross_version_family_duplication_check,
    quantisation_monotonicity,
)

from . import CONFIG, INDEX_DIR, MANIFEST_PATH, fresh_dir


class CompatMatrixTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.work = fresh_dir("compat")
        cls.upgrade = build_legacy_and_upgrade(cls.work, CONFIG,
                                               _manifest())

    def test_matrix_accepts_current_and_rejects_legacy(self):
        report = compatibility_matrix(
            MANIFEST_PATH, INDEX_DIR,
            self.upgrade["upgraded_dir"],
            self.upgrade["legacy_dir"], CONFIG,
        )
        self.assertEqual(report["summary"]["unexpected"], 0)
        statuses = {
            (c["index_format"], c["query_format"]): c
            for c in report["matrix"]
        }
        self.assertEqual(statuses[("2.0.0", "pcm")]["hit_rate"], 1.0)
        self.assertEqual(statuses[("2.0.0", "lossy")]["hit_rate"], 1.0)
        self.assertEqual(
            statuses[("upgraded-2.0.0", "lossy")]["hit_rate"], 1.0)
        self.assertEqual(statuses[("1.0.0", "pcm")]["status"], "rejected")
        self.assertEqual(
            statuses[("1.0.0", "pcm")]["rejection"]["code"], "E_VER")
        mismatch = next(c for c in report["matrix"]
                        if c["query_format"] == "parameter-mismatch")
        self.assertEqual(mismatch["rejection"]["code"], "E_VER")

    def test_coexistence_dedup_counts_physical_segment_once(self):
        mixed_dir = f"{self.work}/mixed"
        _build_mixed(mixed_dir, self.work, self.upgrade["legacy_dir"])
        report = cross_version_family_duplication_check(
            MANIFEST_PATH, mixed_dir, CONFIG)
        for row in report["rows"]:
            self.assertEqual(row["family_count"], 1)
            self.assertEqual(row["duplicate_physical_keys"], 0)

    def test_quantisation_monotonicity_observable(self):
        report = quantisation_monotonicity(MANIFEST_PATH, CONFIG)
        self.assertTrue(report["pairs"])
        for row in report["pairs"]:
            self.assertTrue(row["monotonic_non_increasing"])
            self.assertTrue(row["meets_floor_at_default"])
            rates = row["rates"]
            self.assertGreaterEqual(rates["1"], rates["2"])
            self.assertGreaterEqual(rates["2"], rates["4"])


def _manifest():
    from afpipe.manifest import load_manifest

    return load_manifest(MANIFEST_PATH)


def _build_mixed(mixed_dir, work, legacy_dir):
    from afpipe.cli import _build_mixed_store
    from afpipe.manifest import load_manifest

    manifest = load_manifest(MANIFEST_PATH)
    _build_mixed_store(
        type("Args", (), {"work": work})(),
        CONFIG, manifest, mixed_dir, legacy_dir,
    )


if __name__ == "__main__":
    unittest.main()
