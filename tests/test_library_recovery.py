from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

from tests.helpers import PROJECT_ROOT

from fpstore.audio import generate_single_sample
from fpstore.config import FingerprintConfig
from fpstore.errors import BizError, E_BUSY, E_LIB_MISMATCH, E_RECOVERY_INVALID
from fpstore.library import Library, OperationLock, TrackSpec
from fpstore.util import canonical_bytes, content_hash, hash_label


class LibraryRecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = Path(tempfile.mkdtemp(prefix="fpstore-lib-"))
        self.a = generate_single_sample(self.temp / "a.wav", seed=31, sample_rate=8000, duration_s=4.0)
        self.b = generate_single_sample(self.temp / "b.wav", seed=32, sample_rate=16000, duration_s=4.0, stereo=True)
        self.v3 = FingerprintConfig(version=3, time_quant_bits=0)

    def tearDown(self) -> None:
        shutil.rmtree(self.temp, ignore_errors=True)

    def _tracks(self, library: Library, config: FingerprintConfig | None = None) -> list[TrackSpec]:
        cfg = config or self.v3
        return [
            TrackSpec("a", self.a, cfg),
            TrackSpec("b", self.b, cfg),
        ]

    def _cli_import(self, library: Library, *extra: str) -> subprocess.CompletedProcess[bytes]:
        return subprocess.run(
            [
                sys.executable,
                "-m",
                "fpstore.cli",
                "import",
                "--library",
                str(library.root),
                "--samples-dir",
                str(self.temp),
                "--track-id-from-stem",
                "--version",
                "3",
                "--quant-bits",
                "0",
                *extra,
            ],
            cwd=PROJECT_ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_forced_termination_restart_skips_committed_segments(self) -> None:
        library = Library(self.temp / "library-crash")
        crashed = self._cli_import(library, "--crash-after", "1")
        self.assertEqual(crashed.returncode, 137, crashed.stderr.decode(errors="replace"))
        details = library.staging_details()
        assert details is not None
        self.assertEqual(details["entry_count"], 1)
        self.assertIsNone(details["published_version_hash"])
        first_key = details["committed_segments"][0]["key"]

        result = library.import_tracks([], resume=True)
        self.assertEqual(result.skipped_segments, 1)
        self.assertEqual(result.committed_segments, 3)
        resumed = library.staging_details()
        assert resumed is not None
        self.assertEqual(resumed["entry_count"], 4)
        self.assertEqual(len({entry["key"] for entry in resumed["committed_segments"]}), 4)
        self.assertIn(first_key, {entry["key"] for entry in resumed["committed_segments"]})

        published = library.publish()
        self.assertEqual(published["entry_count"], 4)
        query = library.search(self.a, config=self.v3, top_k=2)
        self.assertEqual(query["candidates"][0]["sample"], "a")
        self.assertTrue(query["snapshot_released"])

    def test_cooperative_cancel_keeps_last_published_version(self) -> None:
        library = Library(self.temp / "library-cancel")
        library.import_tracks([TrackSpec("a", self.a, self.v3)])
        first = library.publish()
        cancelled = library.import_tracks(self._tracks(library), cancel_after=1)
        self.assertEqual(cancelled.status, "cancelled")
        self.assertEqual(library.status()["published"]["manifest_hash"], first["published_version_hash"])
        self.assertEqual(library.staging_details()["state"], "cancelled")  # type: ignore[index]
        resumed = library.import_tracks([], resume=True)
        self.assertGreaterEqual(resumed.skipped_segments, 1)
        second = library.publish()
        self.assertNotEqual(first["published_version_hash"], second["published_version_hash"])

    def test_second_worker_gets_e_busy(self) -> None:
        library = Library(self.temp / "library-busy")
        with OperationLock(library.lock_path):
            with self.assertRaises(BizError) as caught:
                library.import_tracks(self._tracks(library))
        self.assertEqual(caught.exception.code, E_BUSY)

    def test_publish_is_whole_snapshot_and_old_snapshot_stays_complete(self) -> None:
        library = Library(self.temp / "library-publish")
        library.import_tracks(self._tracks(library))
        first = library.publish()
        old_snapshot = library.open_snapshot()
        old_records = len(old_snapshot.records)
        self.assertEqual(old_snapshot.library_version_hash, first["published_version_hash"])

        v2 = FingerprintConfig(version=2, time_quant_bits=0)
        library.import_tracks(self._tracks(library, v2))
        before_publish = library.search(self.a, config=v2, top_k=2)
        self.assertEqual(before_publish["library_version_hash"], first["published_version_hash"])
        second = library.publish()
        self.assertNotEqual(first["published_version_hash"], second["published_version_hash"])
        new_snapshot = library.open_snapshot()
        self.assertEqual(new_snapshot.library_version_hash, second["published_version_hash"])
        self.assertGreater(len(new_snapshot.records), old_records)
        self.assertFalse(old_snapshot.closed)
        self.assertEqual(len(old_snapshot.records), old_records)
        old_snapshot.close()
        new_snapshot.close()
        after_publish = library.search(self.a, config=v2, top_k=3)
        self.assertEqual(after_publish["library_version_hash"], second["published_version_hash"])
        self.assertIn("a", {candidate["sample"] for candidate in after_publish["candidates"]})

    def test_concurrent_query_sees_only_complete_old_or_new_version(self) -> None:
        library = Library(self.temp / "library-concurrent")
        library.import_tracks([TrackSpec("a", self.a, self.v3)])
        first = library.publish()
        v2 = FingerprintConfig(version=2, time_quant_bits=0)
        library.import_tracks([TrackSpec("a", self.a, v2)])
        old_count = 2
        observed: list[tuple[str, int]] = []
        failure: list[BaseException] = []
        start = threading.Event()

        def publish_worker() -> None:
            start.wait()
            try:
                time.sleep(0.005)
                library.publish()
            except BaseException as exc:  # pragma: no cover - assertion helper
                failure.append(exc)

        thread = threading.Thread(target=publish_worker)
        thread.start()
        start.set()
        while thread.is_alive():
            snapshot = library.open_snapshot()
            observed.append((snapshot.library_version_hash, len(snapshot.records)))
            snapshot.close()
        thread.join()
        if failure:
            raise failure[0]
        snapshot = library.open_snapshot()
        new_hash = snapshot.library_version_hash
        new_count = len(snapshot.records)
        snapshot.close()
        self.assertNotEqual(first["published_version_hash"], new_hash)
        self.assertGreater(len(observed), 0)
        allowed = {
            (first["published_version_hash"], old_count),
            (new_hash, new_count),
        }
        self.assertTrue(set(observed).issubset(allowed), observed)

    def test_tampered_pointer_returns_e_lib_mismatch_and_expected_hash_is_checked(self) -> None:
        library = Library(self.temp / "library-mismatch")
        library.import_tracks(self._tracks(library))
        published = library.publish()
        self.assertEqual(
            library.status()["published"]["manifest_hash"],
            published["published_version_hash"],
        )
        with self.assertRaises(BizError) as caught:
            library.search(self.a, config=self.v3, expected_version_hash="sha256:" + "0" * 64)
        self.assertEqual(caught.exception.code, E_LIB_MISMATCH)

        pointer_path = library.published_pointer
        pointer = json.loads(pointer_path.read_text())
        pointer["manifest_hash"] = "sha256:" + "1" * 64
        pointer_path.write_bytes(canonical_bytes(pointer))
        with self.assertRaises(BizError) as caught:
            library.open_snapshot()
        self.assertEqual(caught.exception.code, E_LIB_MISMATCH)

    def test_broken_chain_reports_first_segment(self) -> None:
        library = Library(self.temp / "library-chain")
        crashed = self._cli_import(library, "--crash-after", "2")
        self.assertEqual(crashed.returncode, 137, crashed.stderr.decode(errors="replace"))
        pointer = json.loads(library.staging_pointer.read_text())
        manifest_path = library.root / pointer["manifest_file"]
        manifest = json.loads(manifest_path.read_text())
        first = manifest["entries"][0]
        first["chain_hash"] = "sha256:" + "f" * 64
        core = {key: value for key, value in manifest.items() if key != "manifest_hash"}
        manifest["manifest_hash"] = hash_label(content_hash(core))
        manifest_path.write_bytes(canonical_bytes(manifest))
        with self.assertRaises(BizError) as caught:
            library.staging_details()
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(caught.exception.details["first_broken_segment"], first["key"])


if __name__ == "__main__":
    unittest.main()
