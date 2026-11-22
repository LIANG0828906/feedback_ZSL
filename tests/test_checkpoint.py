from __future__ import annotations

import json
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from mediaforge.injection import InjectionConfig
from mediaforge.jsonutil import sha256_bytes
from mediaforge.package import build_package
from mediaforge.segmented import pack_segmented

from .support import SAMPLES, loaded, read_json, write_json


class CheckpointTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.model = loaded("g01_neon_city", "v3").model
        cls.truth = SAMPLES / "g01_neon_city" / "truth.json"

    def test_serial_and_parallel_artifacts_are_identical(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-concurrency-") as temp:
            root = Path(temp)
            serial = pack_segmented(
                self.model,
                "v3",
                root / "serial-checkpoints",
                workers=1,
                parameter_version="p-concurrency",
                artifact_path=root / "serial.mfz",
                truth_path=self.truth,
            )
            parallel = pack_segmented(
                self.model,
                "v3",
                root / "parallel-checkpoints",
                workers=4,
                parameter_version="p-concurrency",
                artifact_path=root / "parallel.mfz",
                truth_path=self.truth,
            )
            self.assertTrue(serial.ok)
            self.assertTrue(parallel.ok)
            self.assertEqual(serial.artifact_sha256, parallel.artifact_sha256)
            self.assertEqual((root / "serial.mfz").read_bytes(), (root / "parallel.mfz").read_bytes())

    def test_direct_and_segmented_artifacts_are_identical(self) -> None:
        direct = build_package(self.model, "v3")
        with tempfile.TemporaryDirectory(prefix="mf-direct-segment-") as temp:
            root = Path(temp)
            segmented = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=3,
                parameter_version="p-direct",
                artifact_path=root / "segmented.mfz",
            )
            self.assertTrue(segmented.ok)
            self.assertEqual(direct.artifact, segmented.artifact)
            self.assertEqual(direct.artifact_sha256, segmented.artifact_sha256)

    def test_concurrent_runs_share_snapshot_without_interleaving(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-concurrent-") as temp:
            root = Path(temp)

            def run(worker_count: int):
                return pack_segmented(
                    self.model,
                    "v3",
                    root / "checkpoints",
                    workers=worker_count,
                    parameter_version="p-shared",
                    artifact_path=root / f"artifact-{worker_count}.mfz",
                )

            with ThreadPoolExecutor(max_workers=2) as executor:
                first, second = [future.result() for future in (executor.submit(run, 2), executor.submit(run, 4))]
            self.assertTrue(first.ok and second.ok)
            self.assertEqual(first.artifact_sha256, second.artifact_sha256)
            self.assertEqual((root / "artifact-2.mfz").read_bytes(), (root / "artifact-4.mfz").read_bytes())
            manifest_path = next((root / "checkpoints").glob("*/manifest.json"))
            manifest = read_json(manifest_path)
            self.assertEqual(manifest["state"], "complete")
            self.assertEqual(manifest["completed_count"], 2)

    def test_worker_limit_is_four(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-workers-") as temp:
            result = pack_segmented(
                self.model,
                "v3",
                temp,
                workers=99,
                parameter_version="p-workers",
            )
        self.assertTrue(result.ok)
        self.assertEqual(result.effective_workers, 4)

    def test_completed_snapshot_is_reused(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-reuse-") as temp:
            root = Path(temp)
            first = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-reuse",
                artifact_path=root / "first.mfz",
            )
            second = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=1,
                parameter_version="p-reuse",
                artifact_path=root / "second.mfz",
            )
            self.assertTrue(first.ok and second.ok)
            self.assertEqual(first.artifact_sha256, second.artifact_sha256)
            self.assertEqual(set(second.reused_tracks), {"audio_main", "video_main"})
            self.assertEqual(second.built_tracks, ())

    def test_single_segment_failure_does_not_pollute_other_segments(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-inject-") as temp:
            root = Path(temp)
            seed = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-retry",
                artifact_path=root / "seed.mfz",
            )
            self.assertTrue(seed.ok)
            corrupt = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-retry",
                injection=InjectionConfig(corrupt_track="audio_main", corrupt_offset=73, parameter_version="p-retry"),
                artifact_path=root / "failed.mfz",
            )
            self.assertFalse(corrupt.ok)
            self.assertEqual(len(corrupt.failures), 1)
            self.assertEqual(corrupt.failures[0].code, "E_CORRUPT")
            self.assertEqual(corrupt.failures[0].track_id, "audio_main")
            self.assertFalse((root / "failed.mfz").exists())
            self.assertIn("video_main", corrupt.reused_tracks)
            manifest = json.loads((Path(temp) / "checkpoints" / corrupt.snapshot_id / "manifest.json").read_text())
            self.assertEqual(manifest["segments"]["video_main"]["status"], "complete")
            self.assertEqual(manifest["segments"]["audio_main"]["status"], "failed")
            retry = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-retry",
                artifact_path=root / "retry.mfz",
            )
            self.assertTrue(retry.ok)
            self.assertIn("video_main", retry.reused_tracks)
            self.assertEqual(retry.built_tracks, ("audio_main",))
            self.assertEqual(retry.artifact_sha256, seed.artifact_sha256)

    def test_external_segment_corruption_invalidates_whole_snapshot(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-invalidate-") as temp:
            root = Path(temp)
            first = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-invalidate",
                artifact_path=root / "first.mfz",
            )
            self.assertTrue(first.ok)
            snapshot_dir = root / "checkpoints" / first.snapshot_id
            manifest = json.loads((snapshot_dir / "manifest.json").read_text())
            segment_file = next(
                segment["file"]
                for segment in manifest["segments"].values()
                if segment["status"] == "complete"
            )
            path = snapshot_dir / segment_file
            data = bytearray(path.read_bytes())
            data[-40] ^= 0x11
            path.write_bytes(bytes(data))
            rebuilt = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-invalidate",
                artifact_path=root / "rebuilt.mfz",
            )
            self.assertTrue(rebuilt.ok)
            self.assertTrue(rebuilt.invalidated_snapshot)
            self.assertEqual(rebuilt.reused_tracks, ())
            self.assertEqual(set(rebuilt.built_tracks), {"audio_main", "video_main"})
            self.assertEqual(rebuilt.artifact_sha256, first.artifact_sha256)

    def test_manifest_version_mismatch_invalidates_snapshot(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-version-") as temp:
            root = Path(temp)
            first = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=1,
                parameter_version="p-version",
            )
            snapshot_dir = root / "checkpoints" / first.snapshot_id
            manifest_path = snapshot_dir / "manifest.json"
            manifest = read_json(manifest_path)
            manifest["algorithm_version"] = "tampered-algorithm"
            write_json(manifest_path, manifest)
            rebuilt = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                parameter_version="p-version",
            )
            self.assertTrue(rebuilt.ok)
            self.assertTrue(rebuilt.invalidated_snapshot)
            self.assertEqual(rebuilt.reused_tracks, ())

    def test_buffer_limit_fails_without_partial_artifact(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-buffer-") as temp:
            root = Path(temp)
            result = pack_segmented(
                self.model,
                "v3",
                root / "checkpoints",
                workers=2,
                per_thread_buffer_limit=1024,
                parameter_version="p-buffer",
                artifact_path=root / "artifact.mfz",
            )
            self.assertFalse(result.ok)
            self.assertEqual(len(result.failures), 2)
            self.assertTrue(all(failure.code == "E_BUFFER" for failure in result.failures))
            self.assertFalse((root / "artifact.mfz").exists())


if __name__ == "__main__":
    unittest.main()
