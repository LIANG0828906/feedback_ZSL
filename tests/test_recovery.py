from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from tactical.changes import SetDanger, SetObstacle
from tactical.errors import E_RECOVERY_INVALID, ServiceError
from tactical.persistence import SnapshotStore
from tactical.service import TacticalService

from .helpers import open_grid


class RecoveryTests(unittest.TestCase):
    def _build_service(self, root: Path) -> TacticalService:
        service = TacticalService(
            open_grid(18, 18),
            {0: (15, 16), 1: (16, 16), 2: (17, 16)},
            cache_queries=[(0, (5, 0)), (1, (6, 0)), (2, (7, 0))],
            store=root,
        )
        service.commit_batch([SetObstacle(8, 8, True), SetDanger(9, 9, True)])
        service.commit_batch([SetObstacle(8, 8, False), SetDanger(9, 9, False)])
        service.close()
        return service

    def test_recovers_latest_complete_published_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = self._build_service(root)
            expected = source.snapshot
            recovered = TacticalService.recover(root)
            try:
                self.assertEqual(recovered.version, expected.version)
                self.assertEqual(recovered.version_hash, expected.version_hash)
                self.assertEqual(
                    recovered.snapshot.field_hashes(), expected.field_hashes()
                )
                self.assertEqual(
                    set(recovered.snapshot.path_cache), set(expected.path_cache)
                )
            finally:
                recovered.close()

    def test_corrupt_latest_snapshot_fails_recovery(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._build_service(root)
            latest = max(root.glob("snapshot-*.snap"))
            data = bytearray(latest.read_bytes())
            data[len(data) // 2] ^= 0xFF
            latest.write_bytes(bytes(data))
            with self.assertRaises(ServiceError) as raised:
                TacticalService.recover(root)
            self.assertEqual(raised.exception.code, E_RECOVERY_INVALID)

    def test_missing_middle_snapshot_breaks_hash_chain(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._build_service(root)
            middle = root / "snapshot-000000000001.snap"
            middle.unlink()
            with self.assertRaises(ServiceError) as raised:
                SnapshotStore(root).recover_latest()
            self.assertEqual(raised.exception.code, E_RECOVERY_INVALID)

    def test_missing_field_array_fails_recovery(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._build_service(root)
            latest = max(root.glob("snapshot-*.snap"))
            with np.load(latest, allow_pickle=False) as archive:
                arrays = {key: np.asarray(archive[key]) for key in archive.files}
            arrays.pop("field_0")
            with latest.open("wb") as handle:
                np.savez(handle, **arrays)
            with self.assertRaises(ServiceError) as raised:
                SnapshotStore(root).recover_latest()
            self.assertEqual(raised.exception.code, E_RECOVERY_INVALID)

    def test_leftover_temporary_file_is_ignored(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = self._build_service(root)
            expected_hash = source.snapshot.version_hash
            (root / ".snapshot-interrupted.tmp").write_bytes(b"partial")
            recovered = SnapshotStore(root).recover_latest()
            self.assertEqual(recovered.version_hash, expected_hash)


if __name__ == "__main__":
    unittest.main()
