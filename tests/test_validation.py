from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
import wave
from pathlib import Path

from bangzi_align.constants import (
    E_BIT_DEPTH,
    E_CHANNEL_COUNT,
    E_DUPLICATE_VOICE,
    E_FILE_MISSING,
    E_HIT_ORDER,
    E_SAMPLE_RATE,
    E_SEGMENT_OVERLAP,
)
from bangzi_align.errors import DiagnosticError
from bangzi_align.manifest import load_manifest, load_sources
from bangzi_align.synth import generate_demo


class ValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory(prefix="bangzi-validation-")
        cls.root = Path(cls._temporary.name)
        cls.base = cls.root / "base"
        generate_demo(cls.base, seed=237)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def _case(self, name: str) -> tuple[Path, dict]:
        case = self.root / name
        shutil.copytree(self.base, case)
        manifest_path = case / "manifest.json"
        payload = json.loads(manifest_path.read_text(encoding="utf-8"))
        return case, payload

    def _write_manifest(self, case: Path, payload: dict) -> Path:
        path = case / "manifest.json"
        path.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        return path

    def _diagnostics(self, manifest_path: Path) -> list:
        with self.assertRaises(DiagnosticError) as caught:
            manifest = load_manifest(manifest_path)
            load_sources(manifest)
        return caught.exception.diagnostics

    def test_generation_is_byte_deterministic(self) -> None:
        first = self.root / "deterministic-a"
        second = self.root / "deterministic-b"
        generate_demo(first, seed=731)
        generate_demo(second, seed=731)
        self.assertEqual(
            (first / "manifest.json").read_bytes(),
            (second / "manifest.json").read_bytes(),
        )
        for channel in ("bangzi", "banhu", "yueqin", "laosheng", "bangqiang"):
            self.assertEqual(
                (first / "audio" / f"{channel}.wav").read_bytes(),
                (second / "audio" / f"{channel}.wav").read_bytes(),
            )
        first_manifest = (first / "manifest.json").read_bytes()
        first_bangzi = (first / "audio" / "bangzi.wav").read_bytes()
        generate_demo(first, seed=731)
        self.assertEqual((first / "manifest.json").read_bytes(), first_manifest)
        self.assertEqual((first / "audio" / "bangzi.wav").read_bytes(), first_bangzi)

    def test_sample_rate_mismatch(self) -> None:
        case, payload = self._case("rate")
        payload["audio"]["sample_rate"] = 44_100
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_SAMPLE_RATE, {item.code for item in diagnostics})
        self.assertTrue(all(item.channel and item.code for item in diagnostics))

    def test_bit_depth_mismatch(self) -> None:
        case, payload = self._case("depth")
        payload["audio"]["bit_depth"] = 16
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_BIT_DEPTH, {item.code for item in diagnostics})

    def test_missing_file(self) -> None:
        case, payload = self._case("missing")
        payload["channels"][0]["path"] = "audio/not-there.wav"
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_FILE_MISSING, {item.code for item in diagnostics})
        target = [item for item in diagnostics if item.code == E_FILE_MISSING]
        self.assertTrue(any(item.channel == "bangzi" for item in target))

    def test_wav_channel_count_mismatch(self) -> None:
        case, payload = self._case("channels")
        source = case / "audio" / "bangzi.wav"
        with wave.open(str(source), "rb") as handle:
            parameters = handle.getparams()
            raw = handle.readframes(handle.getnframes())
        bytes_per_sample = parameters.sampwidth
        stereo = bytearray()
        for offset in range(0, len(raw), bytes_per_sample):
            frame = raw[offset : offset + bytes_per_sample]
            stereo.extend(frame)
            stereo.extend(frame)
        with wave.open(str(source), "wb") as handle:
            handle.setnchannels(2)
            handle.setsampwidth(parameters.sampwidth)
            handle.setframerate(parameters.framerate)
            handle.writeframes(bytes(stereo))
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_CHANNEL_COUNT, {item.code for item in diagnostics})

    def test_duplicate_voice(self) -> None:
        case, payload = self._case("duplicate-voice")
        payload["channels"][1]["voice"] = "梆子"
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_DUPLICATE_VOICE, {item.code for item in diagnostics})

    def test_segment_overlap(self) -> None:
        case, payload = self._case("overlap")
        payload["segments"][1]["start_sample"] = payload["segments"][0]["end_sample"] - 1_000
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_SEGMENT_OVERLAP, {item.code for item in diagnostics})

    def test_reference_order(self) -> None:
        case, payload = self._case("reference-order")
        references = payload["segments"][0]["bangzi_reference_samples"]
        references[0], references[1] = references[1], references[0]
        diagnostics = self._diagnostics(self._write_manifest(case, payload))
        self.assertIn(E_HIT_ORDER, {item.code for item in diagnostics})

    def test_cli_validation_error_is_structured_and_nonzero(self) -> None:
        case, payload = self._case("cli-error")
        payload["channels"][0]["path"] = "audio/not-there.wav"
        manifest_path = self._write_manifest(case, payload)
        result = subprocess.run(
            [sys.executable, "-m", "bangzi_align", "validate", "--manifest", str(manifest_path)],
            cwd=Path(__file__).resolve().parents[1],
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertNotEqual(result.returncode, 0)
        diagnostic = json.loads(result.stderr)
        self.assertFalse(diagnostic["ok"])
        item = diagnostic["diagnostics"][0]
        self.assertEqual(item["code"], E_FILE_MISSING)
        self.assertEqual(item["channel"], "bangzi")


if __name__ == "__main__":
    unittest.main()

