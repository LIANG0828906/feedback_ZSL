"""End-to-end tests: format parsing/validation, migration, retrieval
consistency, persistence and error codes. Uses deterministic local
synthetic samples only."""

import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from fpworkbench.consistency import compare_results, run_queries
from fpworkbench.errors import (E_BASELINE_MISMATCH, E_BUSY, E_FMT_INVALID,
                                E_LIB_MISMATCH, E_MIGRATE_LOSS, FpError)
from fpworkbench.extract import extract_fingerprint
from fpworkbench.formats import load_fingerprint, save_fingerprint
from fpworkbench.library import Library
from fpworkbench.migrate import migrate_fingerprint
from fpworkbench.retrieve import common_baseline, search
from fpworkbench.samplegen import generate_corpus

SEED = 42


class CorpusMixin(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = tempfile.mkdtemp(prefix="fpwb_test_")
        cls.manifest = generate_corpus(cls.root, seed=SEED)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.root, ignore_errors=True)

    def path(self, rel):
        return os.path.join(self.root, rel)


class TestFormats(CorpusMixin):
    def test_parse_all_three_versions(self):
        for s in self.manifest["samples"]:
            fp = load_fingerprint(self.path(s["baseline"]))
            self.assertEqual(fp.sample_id, s["sample_id"])
            self.assertEqual(fp.source_version, s["version"])
            self.assertTrue(fp.hashes, "hashes must not be empty")
            self.assertTrue(fp.peak_pairs, "peak pairs must not be empty")

    def test_roundtrip_preserves_content(self):
        for version in (1, 2, 3):
            fp = extract_fingerprint(self.path("wav/track_beta.wav"), "rt")
            p = self.path(f"rt.v{version}.json")
            save_fingerprint(fp, p, version=version)
            back = load_fingerprint(p)
            self.assertEqual(back.sample_id, "rt")
            self.assertEqual(back.sample_rate, fp.sample_rate)
            self.assertEqual(len(back.peak_pairs), len(fp.peak_pairs))
            self.assertEqual(len(back.hashes), len(fp.hashes))

    def test_missing_field_raises_fmt_invalid_with_path(self):
        fp = extract_fingerprint(self.path("wav/track_alpha.wav"), "x")
        p = self.path("bad_missing.json")
        save_fingerprint(fp, p, version=1)
        doc = json.load(open(p))
        del doc["payload"]["peak_pairs"]
        from fpworkbench.formats.common import compute_checksum
        doc["checksum"] = compute_checksum(doc["payload"])
        json.dump(doc, open(p, "w"))
        with self.assertRaises(FpError) as ctx:
            load_fingerprint(p)
        self.assertEqual(ctx.exception.code, E_FMT_INVALID)
        self.assertEqual(ctx.exception.field, "payload.peak_pairs")
        self.assertEqual(ctx.exception.file, p)

    def test_checksum_mismatch(self):
        fp = extract_fingerprint(self.path("wav/track_alpha.wav"), "x")
        p = self.path("bad_checksum.json")
        save_fingerprint(fp, p, version=2)
        doc = json.load(open(p))
        doc["payload"]["channels"] = 9
        json.dump(doc, open(p, "w"))
        with self.assertRaises(FpError) as ctx:
            load_fingerprint(p)
        self.assertEqual(ctx.exception.code, E_FMT_INVALID)
        self.assertEqual(ctx.exception.field, "checksum")

    def test_unknown_version(self):
        p = self.path("bad_version.json")
        json.dump({"format_version": 42, "payload": {}, "checksum": "x"},
                  open(p, "w"))
        with self.assertRaises(FpError) as ctx:
            load_fingerprint(p)
        self.assertEqual(ctx.exception.code, E_FMT_INVALID)
        self.assertEqual(ctx.exception.field, "format_version")

    def test_v3_channel_merge_and_compression_fields(self):
        fp = extract_fingerprint(self.path("wav/track_gamma.wav"), "g")
        self.assertTrue(fp.channel_merge)  # stereo source
        fp.compression = {"codec": "flac-like", "level": 5,
                          "original_bytes": 1234}
        p = self.path("g.v3.json")
        save_fingerprint(fp, p, version=3)
        back = load_fingerprint(p)
        self.assertTrue(back.channel_merge)
        self.assertEqual(back.compression["codec"], "flac-like")
        doc = json.load(open(p))
        del doc["payload"]["compression"]["codec"]
        from fpworkbench.formats.common import compute_checksum
        doc["checksum"] = compute_checksum(doc["payload"])
        json.dump(doc, open(p, "w"))
        with self.assertRaises(FpError) as ctx:
            load_fingerprint(p)
        self.assertEqual(ctx.exception.code, E_FMT_INVALID)
        self.assertEqual(ctx.exception.field, "payload.compression.codec")


class TestMigration(CorpusMixin):
    def test_v1_to_v3_preserves_peak_pairs_and_reports(self):
        fp = load_fingerprint(self.path("baselines/track_alpha.v1.json"))
        new, report = migrate_fingerprint(fp, 3)
        self.assertEqual(new.source_version, 3)
        self.assertEqual(len(new.peak_pairs), len(fp.peak_pairs))
        self.assertEqual(new.legacy["source_version"], 1)
        self.assertEqual(len(new.hashes), len(fp.hashes))
        self.assertTrue(report.field_mappings)
        self.assertTrue(all(l["code"] == E_MIGRATE_LOSS
                            for l in report.lost_fields))
        self.assertTrue(any(l["field"] == "payload.channel_merge"
                            for l in report.lost_fields))

    def test_v2_to_v3_keeps_quant_baseline_in_legacy(self):
        fp = load_fingerprint(self.path("baselines/track_beta.v2.json"))
        new, report = migrate_fingerprint(fp, 3)
        self.assertEqual(new.legacy["time_quant_ms"], fp.time_quant_ms)
        self.assertEqual(new.legacy["quant_bits"], fp.quant_bits)
        self.assertTrue(any(l["field"] == "payload.quant_bits"
                            for l in report.lost_fields))
        p = self.path("migrated_beta.v3.json")
        save_fingerprint(new, p, version=3)
        back = load_fingerprint(p)
        self.assertEqual(back.source_version, 3)
        self.assertEqual(back.sample_id, fp.sample_id)

    def test_migrated_v3_roundtrip_stable(self):
        fp = load_fingerprint(self.path("baselines/track_alpha.v1.json"))
        new, _ = migrate_fingerprint(fp, 3)
        p = self.path("mig_alpha.v3.json")
        save_fingerprint(new, p, version=3)
        again = load_fingerprint(p)
        self.assertEqual(again.hashes, new.hashes)


class TestRetrieval(CorpusMixin):
    def _library(self):
        lib = Library(self.root)
        for s in self.manifest["samples"]:
            lib.add_fingerprint_file(self.path(s["baseline"]))
        lib.save()
        return lib

    def test_queries_hit_source_at_truth_offset(self):
        lib = self._library()
        entries = lib.load_fingerprints()
        for q in self.manifest["queries"]:
            qfp = extract_fingerprint(self.path(q["wav"]), q["query_id"])
            cands = search(qfp, entries)
            self.assertTrue(cands, f"no candidates for {q['query_id']}")
            hit = next((c for c in cands
                        if c["sample_id"] == q["source_sample"]), None)
            self.assertIsNotNone(hit, f"source not matched for {q['query_id']}")
            err = abs(hit["offset_ms"] - q["truth_offset_ms"])
            self.assertLessEqual(err, 64.0,
                                 f"offset error {err}ms for {q['query_id']}")
            self.assertGreater(hit["confidence"], 0.3)

    def test_baseline_mismatch_aborts(self):
        fp = extract_fingerprint(self.path("wav/track_beta.wav"), "b48")
        fp.time_quant_ms = 48.0
        fp.hashes = [(h, round(round(t / 48.0) * 48.0, 6))
                     for h, t in fp.hashes]
        q = extract_fingerprint(self.path("wav/query_beta.wav"), "q")
        with self.assertRaises(FpError) as ctx:
            search(q, [fp])
        self.assertEqual(ctx.exception.code, E_BASELINE_MISMATCH)

    def test_integer_ratio_baselines_align(self):
        self.assertEqual(common_baseline(32.0, 64.0), 64.0)
        self.assertEqual(common_baseline(32.0, 32.0), 32.0)
        with self.assertRaises(FpError):
            common_baseline(32.0, 48.0)

    def test_consistency_across_migration(self):
        lib = self._library()
        queries = [extract_fingerprint(self.path(q["wav"]), q["query_id"])
                   for q in self.manifest["queries"]]
        before = run_queries(queries, lib.load_fingerprints())
        lib.migrate_all()
        after = run_queries(queries, lib.load_fingerprints())
        report = compare_results(before, after, 64.0, "mixed", "v3")
        self.assertTrue(report["passed"], json.dumps(report, indent=1))
        for d in report["details"]:
            self.assertEqual(d["status"], "OK")
            self.assertLessEqual(d["delta_ms"], 64.0)


class TestLibrary(CorpusMixin):
    def test_atomic_save_and_load(self):
        lib = Library(self.root)
        for s in self.manifest["samples"]:
            lib.add_fingerprint_file(self.path(s["baseline"]))
        lib.save()
        self.assertFalse(os.path.exists(lib.manifest_path + ".tmp"))
        loaded = Library(self.root).load()
        self.assertEqual(len(loaded.samples), 3)
        self.assertEqual(loaded.version_distribution(), {1: 1, 2: 1, 3: 1})

    def test_version_mismatch_aborts(self):
        lib = Library(self.root)
        lib.save()
        doc = json.load(open(lib.manifest_path))
        doc["lib_version"] = 99
        json.dump(doc, open(lib.manifest_path, "w"))
        with self.assertRaises(FpError) as ctx:
            Library(self.root).load()
        self.assertEqual(ctx.exception.code, E_LIB_MISMATCH)
        self.assertEqual(ctx.exception.field, "lib_version")

    def test_content_hash_mismatch_aborts(self):
        lib = Library(self.root)
        for s in self.manifest["samples"]:
            lib.add_fingerprint_file(self.path(s["baseline"]))
        lib.save()
        doc = json.load(open(lib.manifest_path))
        doc["samples"][0]["sha256"] = "0" * 64
        json.dump(doc, open(lib.manifest_path, "w"))
        with self.assertRaises(FpError) as ctx:
            Library(self.root).load()
        self.assertEqual(ctx.exception.code, E_LIB_MISMATCH)
        self.assertEqual(ctx.exception.field, "content_hash")

    def test_migration_keeps_originals_and_is_exclusive(self):
        lib = Library(self.root)
        for s in self.manifest["samples"]:
            lib.add_fingerprint_file(self.path(s["baseline"]))
        lib.save()
        originals = {s["sample_id"]: s["path"] for s in lib.samples}
        reports = lib.migrate_all()
        self.assertEqual(len(reports), 2)  # v1 + v2 samples
        for s in lib.samples:
            self.assertEqual(s["version"], 3)
            if s["migrated"]:
                self.assertEqual(s["original_path"],
                                 originals[s["sample_id"]])
                self.assertTrue(os.path.exists(
                    self.path(s["original_path"])))
        lib._migrate_lock.acquire()
        try:
            with self.assertRaises(FpError) as ctx:
                lib.migrate_all()
            self.assertEqual(ctx.exception.code, E_BUSY)
        finally:
            lib._migrate_lock.release()


class TestDeterminism(CorpusMixin):
    def test_same_seed_same_fingerprints(self):
        other = tempfile.mkdtemp(prefix="fpwb_det_")
        try:
            generate_corpus(other, seed=SEED)
            for name in ("track_alpha", "track_beta", "track_gamma"):
                fp1 = extract_fingerprint(self.path(f"wav/{name}.wav"), name)
                fp2 = extract_fingerprint(os.path.join(other, "wav",
                                                       f"{name}.wav"), name)
                self.assertEqual(fp1.hashes, fp2.hashes)
        finally:
            shutil.rmtree(other, ignore_errors=True)


if __name__ == "__main__":
    unittest.main(verbosity=2)
