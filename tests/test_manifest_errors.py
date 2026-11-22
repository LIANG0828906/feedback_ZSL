from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from audio_sync.errors import E_DECODE, E_SCHEMA, SyncError
from audio_sync.generator import SyntheticSessionConfig, generate_session
from audio_sync.manifest import load_manifest, load_session, validate_session


class ManifestValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="audio-sync-manifest-")
        cls.root = Path(cls.temp.name)
        cls.golden = cls.root / "golden"
        generate_session(cls.golden, config=SyntheticSessionConfig(session_id="manifest-test", seed=42))

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _copy_session(self, name: str) -> Path:
        target = self.root / name
        shutil.copytree(self.golden, target)
        return target

    def _read_manifest(self, path: Path) -> dict:
        return json.loads((path / "manifest.json").read_text(encoding="utf-8"))

    def _write_manifest(self, path: Path, payload: dict) -> None:
        (path / "manifest.json").write_text(
            json.dumps(payload, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )

    def test_valid_loader(self) -> None:
        info = validate_session(self.golden)
        self.assertTrue(info["ok"])
        self.assertEqual(info["channel_count"], 4)
        self.assertGreaterEqual(info["markers"], 10)

    def test_missing_channel_field_is_schema_error_with_context(self) -> None:
        session = self._copy_session("missing-field")
        manifest = self._read_manifest(session)
        del manifest["channels"][1]["samples"]
        self._write_manifest(session, manifest)
        with self.assertRaises(SyncError) as context:
            load_manifest(session)
        error = context.exception
        self.assertEqual(error.code, E_SCHEMA)
        self.assertEqual(error.channel, "capture_a")
        self.assertEqual(error.field, "samples")

    def test_sample_count_mismatch_is_schema_error(self) -> None:
        session = self._copy_session("count-mismatch")
        manifest = self._read_manifest(session)
        manifest["channels"][1]["samples"] += 1
        self._write_manifest(session, manifest)
        with self.assertRaises(SyncError) as context:
            load_session(session)
        self.assertEqual(context.exception.code, E_SCHEMA)
        self.assertEqual(context.exception.channel, "capture_a")
        self.assertEqual(context.exception.field, "samples")

    def test_duplicate_channel_identifier_is_schema_error(self) -> None:
        session = self._copy_session("duplicate")
        manifest = self._read_manifest(session)
        manifest["channels"][2]["id"] = manifest["channels"][1]["id"]
        self._write_manifest(session, manifest)
        with self.assertRaises(SyncError) as context:
            load_manifest(session)
        self.assertEqual(context.exception.code, E_SCHEMA)
        self.assertEqual(context.exception.field, "id")

    def test_different_channel_sample_rate_is_schema_error(self) -> None:
        session = self._copy_session("rate-mismatch")
        manifest = self._read_manifest(session)
        manifest["channels"][1]["sample_rate"] += 1
        self._write_manifest(session, manifest)
        with self.assertRaises(SyncError) as context:
            load_manifest(session)
        self.assertEqual(context.exception.code, E_SCHEMA)
        self.assertEqual(context.exception.channel, "capture_a")
        self.assertEqual(context.exception.field, "sample_rate")

    def test_corrupt_npy_is_decode_error(self) -> None:
        session = self._copy_session("corrupt-npy")
        (session / "channels" / "capture_a.npy").write_bytes(b"not an npy file")
        with self.assertRaises(SyncError) as context:
            load_session(session)
        self.assertEqual(context.exception.code, E_DECODE)
        self.assertEqual(context.exception.channel, "capture_a")

    def test_content_hash_mismatch_is_decode_error(self) -> None:
        session = self._copy_session("hash-mismatch")
        path = session / "channels" / "capture_b.npy"
        data = np.load(path)
        data[100] += np.float32(0.001)
        np.save(path, data)
        with self.assertRaises(SyncError) as context:
            load_session(session)
        self.assertEqual(context.exception.code, E_DECODE)
        self.assertEqual(context.exception.channel, "capture_b")
        self.assertEqual(context.exception.field, "content_sha256")

    def test_truth_fingerprint_mismatch_is_decode_error(self) -> None:
        session = self._copy_session("truth-hash")
        manifest = self._read_manifest(session)
        manifest["truth"]["sha256"] = "0" * 64
        self._write_manifest(session, manifest)
        with self.assertRaises(SyncError) as context:
            load_manifest(session)
        self.assertEqual(context.exception.code, E_DECODE)
        self.assertEqual(context.exception.field, "truth.sha256")

    def test_generator_is_deterministic(self) -> None:
        first = self.root / "deterministic-a"
        second = self.root / "deterministic-b"
        config = SyntheticSessionConfig(session_id="deterministic", seed=987654)
        first_info = generate_session(first, config=config)
        second_info = generate_session(second, config=config)
        self.assertEqual(first_info["truth_sha256"], second_info["truth_sha256"])
        first_manifest = self._read_manifest(first)
        second_manifest = self._read_manifest(second)
        self.assertEqual(
            [item["content_sha256"] for item in first_manifest["channels"]],
            [item["content_sha256"] for item in second_manifest["channels"]],
        )


if __name__ == "__main__":
    unittest.main()

