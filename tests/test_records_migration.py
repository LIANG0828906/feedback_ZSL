from __future__ import annotations

import copy
import shutil
import tempfile
import unittest
from pathlib import Path

from tests.helpers import PROJECT_ROOT  # noqa: F401

from fpstore.audio import generate_single_sample, read_wav
from fpstore.config import FingerprintConfig
from fpstore.errors import BizError, E_FP_INVALID, E_MIGRATE_LOSS
from fpstore.fingerprint import extract_fingerprint
from fpstore.migration import MigrationContext, migrate_record, migrate_records
from fpstore.records import _seal_record, build_record, parse_record, pcm_compression_metadata


class RecordParsingMigrationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = Path(tempfile.mkdtemp(prefix="fpstore-records-"))
        cls.wav = generate_single_sample(cls.temp / "unit.wav", seed=11, sample_rate=8000, duration_s=2.0)
        cls.audio = read_wav(cls.wav)
        cls.v1_config = FingerprintConfig(version=1, time_quant_bits=None)
        cls.v2_config = FingerprintConfig(version=2, time_quant_bits=2)
        cls.v3_config = FingerprintConfig(version=3, time_quant_bits=0)
        cls.v1_fp = extract_fingerprint(cls.audio, cls.v1_config, track_id="unit")
        cls.v2_fp = extract_fingerprint(cls.audio, cls.v2_config, track_id="unit")
        cls.v3_fp = extract_fingerprint(cls.audio, cls.v3_config, track_id="unit")
        cls.v1 = build_record(cls.v1_fp.segments[0], cls.v1_fp, cls.v1_config)
        cls.v2 = build_record(cls.v2_fp.segments[0], cls.v2_fp, cls.v2_config)
        cls.v3 = build_record(cls.v3_fp.segments[0], cls.v3_fp, cls.v3_config)

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.temp, ignore_errors=True)

    def _resealed(self, record, mutate) -> dict:
        raw = copy.deepcopy(record.to_dict())
        mutate(raw)
        return _seal_record(
            raw["version"],
            raw["record_id"],
            raw["payload"],
        )

    def test_v1_derives_hashes_without_inventing_time_fields(self) -> None:
        parsed = parse_record(self.v1.to_dict())
        self.assertEqual(parsed.version, 1)
        self.assertEqual(parsed.time_basis.kind, "frame")
        self.assertNotIn("time_quant_bits", parsed.payload)
        self.assertGreater(len(parsed.hashes), 100)
        self.assertEqual(parsed.hashes[0].hash, parse_record(parsed.to_dict()).hashes[0].hash)

    def test_v2_and_v3_required_fields_are_validated(self) -> None:
        v2_raw = self._resealed(self.v2, lambda raw: raw["payload"].pop("band_layout"))
        with self.assertRaises(BizError) as caught:
            parse_record(v2_raw)
        self.assertEqual(caught.exception.code, E_FP_INVALID)
        self.assertEqual(caught.exception.details["field"], "band_layout")
        self.assertEqual(caught.exception.details["record_id"], self.v2.record_id)

        v3_raw = self._resealed(self.v3, lambda raw: raw["payload"].pop("compression"))
        with self.assertRaises(BizError) as caught:
            parse_record(v3_raw)
        self.assertEqual(caught.exception.code, E_FP_INVALID)
        self.assertEqual(caught.exception.details["field"], "compression")

    def test_unknown_version_and_checksum_mismatch_have_field_locations(self) -> None:
        unknown = copy.deepcopy(self.v3.to_dict())
        unknown["version"] = 9
        with self.assertRaises(BizError) as caught:
            parse_record(unknown)
        self.assertEqual(caught.exception.code, E_FP_INVALID)
        self.assertEqual(caught.exception.details["field"], "version")

        tampered = copy.deepcopy(self.v3.to_dict())
        tampered["payload"]["segment_hashes"][0]["offset_ms"] += 1
        with self.assertRaises(BizError) as caught:
            parse_record(tampered)
        self.assertEqual(caught.exception.code, E_FP_INVALID)
        self.assertEqual(caught.exception.details["field"], "checksum")

    def test_missing_pair_field_is_located(self) -> None:
        raw = self._resealed(self.v3, lambda item: item["payload"]["peak_pairs"][0].pop("f2_bin"))
        with self.assertRaises(BizError) as caught:
            parse_record(raw)
        self.assertEqual(caught.exception.code, E_FP_INVALID)
        self.assertEqual(caught.exception.details["field"], "peak_pairs[0].f2_bin")

    def test_v1_migration_preserves_pairs_hashes_and_frame_basis(self) -> None:
        context = MigrationContext("mono", pcm_compression_metadata(16))
        outcome = migrate_record(self.v1, context)
        self.assertTrue(outcome.ok)
        target = outcome.target_record
        assert target is not None
        self.assertEqual(target.version, 3)
        self.assertEqual(target.time_basis.kind, "frame")
        self.assertEqual(target.payload["origin_time_quantization"]["kind"], "frame")
        self.assertEqual(target.payload["origin_time_quantization"]["frame_number"], self.v1.frame_number)
        self.assertEqual([pair.as_dict() for pair in target.peak_pairs], [pair.as_dict() for pair in self.v1.peak_pairs])
        self.assertEqual([hit.hash for hit in target.hashes], [hit.hash for hit in self.v1.hashes])
        self.assertEqual(target.payload["origin_checksum"], self.v1.checksum)

    def test_v2_migration_preserves_quantization_bits_and_hashes(self) -> None:
        context = MigrationContext("mono", pcm_compression_metadata(16))
        outcome = migrate_record(self.v2, context)
        target = outcome.target_record
        assert target is not None
        self.assertEqual(target.time_basis.kind, "ms")
        self.assertEqual(target.time_basis.bits, 2)
        self.assertEqual(target.payload["origin_time_quantization"]["bits"], 2)
        self.assertEqual(
            [item["hash"] for item in target.payload["segment_hashes"]],
            [item["hash"] for item in self.v2.payload["segment_hashes"]],
        )

    def test_unknown_migration_field_returns_e_migrate_loss(self) -> None:
        raw = self._resealed(
            self.v2,
            lambda item: item["payload"].__setitem__("vendor_confidence", {"score": 7}),
        )
        parsed = parse_record(raw)
        context = MigrationContext("mono", pcm_compression_metadata(16))
        with self.assertRaises(BizError) as caught:
            migrate_records([parsed], context, strict=True)
        self.assertEqual(caught.exception.code, E_MIGRATE_LOSS)
        record_ids = caught.exception.details["affected_records"]
        self.assertEqual(record_ids, [parsed.record_id])
        self.assertEqual(caught.exception.details["lost_fields"][0]["field"], "vendor_confidence")


if __name__ == "__main__":
    unittest.main()
