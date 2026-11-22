import json
import os
import unittest

from afpipe.config import Config
from afpipe.errors import VersionError
from afpipe.hashing import atomic_write_json, read_json
from afpipe.indexstore import (
    INDEX_FORMAT_VERSION,
    LEGACY_INDEX_FORMAT_VERSION,
    IndexStore,
    upgrade_legacy_store,
    write_legacy_v1_store,
)
from afpipe.pipeline import process_file

from . import CONFIG, INDEX_DIR, MANIFEST, fresh_dir


class IndexVersionTest(unittest.TestCase):
    def test_registry_identity_fields(self):
        registry = read_json(os.path.join(INDEX_DIR, "registry.json"))
        self.assertEqual(registry["index_format_version"],
                         INDEX_FORMAT_VERSION)
        self.assertEqual(registry["parameter_version"],
                         CONFIG.parameter_version())
        self.assertEqual(registry["manifest_fingerprint"],
                         MANIFEST.fingerprint)
        for entry in registry["entries"]:
            self.assertEqual(len(entry["content_hash"]), 64)

    def test_parameter_mismatch_refused_with_E_VER(self):
        altered = CONFIG.with_overrides(quant_bits=4)
        with self.assertRaises(VersionError) as ctx:
            IndexStore(INDEX_DIR).load(
                altered, manifest_fingerprint=MANIFEST.fingerprint)
        self.assertEqual(ctx.exception.code, "E_VER")
        self.assertEqual(ctx.exception.details["field"], "parameter_version")

    def test_format_mismatch_refused_with_E_VER(self):
        legacy_dir = fresh_dir("legacy")
        records = [
            (m, process_file(m, MANIFEST, CONFIG).track, m.sha256)
            for m in MANIFEST.index_files()
        ]
        write_legacy_v1_store(legacy_dir, CONFIG,
                              MANIFEST.fingerprint, records)
        with self.assertRaises(VersionError) as ctx:
            IndexStore(legacy_dir).load(
                CONFIG, manifest_fingerprint=MANIFEST.fingerprint)
        self.assertEqual(ctx.exception.code, "E_VER")

    def test_explicit_upgrade_records_mapping_trail(self):
        legacy_dir = fresh_dir("legacy2")
        target_dir = fresh_dir("upgraded2")
        records = [
            (m, process_file(m, MANIFEST, CONFIG).track, m.sha256)
            for m in MANIFEST.index_files()
        ]
        write_legacy_v1_store(legacy_dir, CONFIG,
                              MANIFEST.fingerprint, records)
        result = upgrade_legacy_store(
            legacy_dir, target_dir, CONFIG,
            manifest_fingerprint=MANIFEST.fingerprint,
        )
        mappings = result["trail"]["upgrade"]["mappings"]
        self.assertEqual(len(mappings), len(records))
        self.assertEqual(mappings[0]["legacy_format"],
                         LEGACY_INDEX_FORMAT_VERSION)
        self.assertEqual(mappings[0]["new_format"], INDEX_FORMAT_VERSION)
        store = IndexStore(target_dir).load(
            CONFIG, manifest_fingerprint=MANIFEST.fingerprint)
        self.assertTrue(
            all(e.lineage == "upgraded-from-1.0.0" for e in store.entries))

    def test_atomic_commit_leaves_no_tmp_files(self):
        leftovers = [
            n for n in os.listdir(INDEX_DIR) if n.startswith(".tmp-")
        ]
        self.assertEqual(leftovers, [])

    def test_foreign_manifest_fingerprint_refused(self):
        with self.assertRaises(VersionError) as ctx:
            IndexStore(INDEX_DIR).load(
                CONFIG, manifest_fingerprint="mf-deadbeef")
        self.assertEqual(ctx.exception.details["field"],
                         "manifest_fingerprint")


if __name__ == "__main__":
    unittest.main()
