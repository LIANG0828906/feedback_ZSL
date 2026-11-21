from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from loudness_diag.errors import (
    E_SNAPSHOT_RELEASED,
    E_VERSION_MISMATCH,
    DiagnosticError,
)
from loudness_diag.jsonutil import atomic_write_json, load_json
from loudness_diag.version_store import VersionedStore

from .helpers import make_report


class VersionStoreTests(unittest.TestCase):
    def test_old_version_stays_readable_during_publish_and_handles_release(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = VersionedStore(directory)
            version1 = store.publish(make_report(1))
            old_handle = store.query(version1)
            self.assertEqual(store.active_snapshot_count, 1)
            self.assertEqual(old_handle.report["version_label"], 1)

            version2 = store.publish(make_report(2))
            self.assertNotEqual(version1, version2)
            self.assertEqual(old_handle.report["version_label"], 1)
            current, current_hash = store.query_report()
            self.assertEqual(current["version_label"], 2)
            self.assertEqual(current_hash, version2)
            self.assertEqual(store.current_version_hash, version2)

            report_copy = old_handle.report
            report_copy["version_label"] = 999
            self.assertEqual(old_handle.report["version_label"], 1)
            old_handle.close()
            old_handle.close()
            self.assertEqual(store.active_snapshot_count, 0)
            with self.assertRaises(DiagnosticError) as context:
                _ = old_handle.report
            self.assertEqual(context.exception.code, E_SNAPSHOT_RELEASED)

            reopened = VersionedStore(directory)
            self.assertEqual(reopened.current_version_hash, version2)
            self.assertEqual(reopened.query_report(version1)[0]["version_label"], 1)

    def test_manifest_mismatch_returns_version_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = VersionedStore(directory)
            version = store.publish(make_report(1))
            manifest = load_json(Path(directory) / "publish_manifest.json")
            manifest["current_version_hash"] = "0" * 64
            atomic_write_json(Path(directory) / "publish_manifest.json", manifest)
            with self.assertRaises(DiagnosticError) as context:
                store.query(version)
            self.assertEqual(context.exception.code, E_VERSION_MISMATCH)

    def test_corrupt_version_file_returns_version_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = VersionedStore(directory)
            version = store.publish(make_report(1))
            path = Path(directory) / "versions" / f"{version}.json"
            value = load_json(path)
            value["version_label"] = 42
            atomic_write_json(path, value)
            with self.assertRaises(DiagnosticError) as context:
                store.query(version)
            self.assertEqual(context.exception.code, E_VERSION_MISMATCH)


if __name__ == "__main__":
    unittest.main()

