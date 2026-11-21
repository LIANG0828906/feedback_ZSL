import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from fpkit import experiment as exp
from fpkit.budget import resolve_stage
from fpkit.config import load_config
from fpkit.errors import (AlignMismatch, AudioUnsupported, Busy, BudgetExceeded,
                          LibMismatch, RecoveryInvalid)
from fpkit.fingerprint import content_hash, fingerprint_signal
from fpkit.library import Library
from fpkit.scheduler import BuildScheduler
from fpkit.search import search
from fpkit.tracelog import TraceLog
from fpkit.wavio import read_wav

SEED = 7


class Base(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp(prefix="fpkit-test-"))
        exp.generate(self.dir, SEED, n_samples=6, n_queries=2)
        self.cfg = load_config()

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)

    def gt(self):
        return json.loads((self.dir / "ground_truth.json").read_text())


class TestWav(Base):
    def test_unsupported_fields(self):
        got = {i["field"]: i["error"] for i in exp.check_invalid_audio(self.dir)}
        self.assertEqual(got, {"sample_rate": "E_AUDIO_UNSUPPORTED",
                               "bits_per_sample": "E_AUDIO_UNSUPPORTED",
                               "channels": "E_AUDIO_UNSUPPORTED"})

    def test_read_ok(self):
        sig, rate = read_wav(self.dir / self.gt()["samples"][0]["path"])
        self.assertEqual(rate, self.gt()["samples"][0]["rate"])
        self.assertGreater(len(sig), 0)


class TestFingerprint(Base):
    def test_deterministic_across_stages(self):
        e = self.gt()["samples"][0]
        sig, rate = read_wav(self.dir / e["path"])
        fp = self.cfg["fingerprint"]
        ref, _ = fingerprint_signal(sig, rate, fp)
        for stage in self.cfg["budget"]["degrade_sequence"]:
            rec, _ = fingerprint_signal(
                sig, rate, fp,
                frame_batch=stage.get("frame_batch", fp["frame_batch"]),
                hash_chunk=stage.get("hash_chunk", 0))
            self.assertEqual(rec, ref, f"stage {stage['stage']} changed output")
        self.assertEqual(content_hash(rec), content_hash(ref))


class TestBudget(Base):
    def test_resolve_and_exhaustion(self):
        e = self.gt()["samples"][4]  # 48kHz sample: largest estimate
        sig, rate = read_wav(self.dir / e["path"])
        fp, bd = self.cfg["fingerprint"], self.cfg["budget"]
        stage, est, params, attempts = resolve_stage(
            len(sig), rate, fp, bd, 8 * 1024 * 1024)
        self.assertEqual(stage, 0)
        stage2, est2, _, _ = resolve_stage(len(sig), rate, fp, bd, 400 * 1024)
        self.assertGreater(stage2, 0)
        self.assertLess(est2, est)
        with self.assertRaises(BudgetExceeded):
            resolve_stage(len(sig), rate, fp, bd, 100 * 1024)


class TestSchedulerLibrary(Base):
    def _build(self, cfg=None, trace="trace.jsonl", **kw):
        cfg = cfg or self.cfg
        gt = self.gt()
        specs = [(e["sample_id"], str(self.dir / e["path"]))
                 for e in gt["samples"]]
        tracer = TraceLog(self.dir / "logs" / trace)
        sched = BuildScheduler(cfg, Library(self.dir / "lib"), tracer)
        try:
            return sched.run_batch(kw.get("batch_id", "b1"), 1, specs)
        finally:
            tracer.close()

    def test_busy(self):
        lib = Library(self.dir / "lib")
        lib.acquire_batch("b1")
        try:
            with self.assertRaises(Busy):
                self._build()
        finally:
            lib.release_batch()

    def test_budget_reject_isolation(self):
        import copy
        cfg = copy.deepcopy(self.cfg)
        cfg["scheduler"]["per_task_mem_budget"] = 250 * 1024
        summary = self._build(cfg)
        self.assertGreaterEqual(summary["rejected"], 1)
        lib = Library(self.dir / "lib")
        ids = lib.committed_ids()
        self.assertEqual(len(ids), summary["committed"])
        lib.verify()  # no half-written state

    def test_parallelism_invariant(self):
        import copy
        self._build(trace="t4.jsonl")
        lib4 = (self.dir / "lib" / "manifest.json").read_bytes()
        shutil.rmtree(self.dir / "lib")
        cfg1 = copy.deepcopy(self.cfg)
        cfg1["scheduler"]["parallelism"] = 1
        self._build(cfg1, trace="t1.jsonl")
        lib1 = (self.dir / "lib" / "manifest.json").read_bytes()
        self.assertEqual(lib1, lib4)

    def test_lib_mismatch(self):
        self._build()
        lib = Library(self.dir / "lib")
        sid = lib.committed_ids()[0]
        rec = self.dir / "lib" / "records" / f"{sid}.fp.json"
        obj = json.loads(rec.read_text())
        obj["fp_hash"] = "0" * 64
        rec.write_text(json.dumps(obj))
        with self.assertRaises(LibMismatch) as cm:
            lib.verify()
        self.assertEqual(cm.exception.sample_id, sid)

    def test_recovery_invalid_locates_first_broken(self):
        self._build()
        lib = Library(self.dir / "lib")
        man_path = self.dir / "lib" / "manifest.json"
        man = json.loads(man_path.read_text())
        victim = man["entries"][1]
        victim["fp_hash"] = "f" * 64
        man_path.write_text(json.dumps(man))
        with self.assertRaises(RecoveryInvalid) as cm:
            lib.recover()
        self.assertEqual(cm.exception.sample_id, victim["sample_id"])


class TestRecovery(Base):
    def test_kill_resume_matches_uninterrupted(self):
        import copy
        # uninterrupted reference
        ref = self.dir / "ref"
        exp.generate(ref, SEED, n_samples=6, n_queries=2)
        cfg = load_config()
        (ref / "config.json").write_text(json.dumps(cfg))
        exp.build(ref, cfg)
        ref_lib = (ref / "lib" / "manifest.json").read_bytes()
        # killed + resumed
        wk = self.dir / "kill"
        exp.generate(wk, SEED, n_samples=6, n_queries=2)
        (wk / "config.json").write_text(json.dumps(cfg))
        proc = subprocess.run(
            [sys.executable, "-m", "fpkit.cli", "build", "--workdir", str(wk),
             "--kill-after", "3"], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 137)
        lib = Library(wk / "lib")
        self.assertEqual(len(lib.recover()), 3)
        exp.build(wk, cfg, trace_name="trace_resume.jsonl")
        self.assertEqual((wk / "lib" / "manifest.json").read_bytes(), ref_lib)
        self.assertEqual(len(lib.committed_ids()), 6)


class TestSearch(Base):
    def test_query_hits_and_version(self):
        exp.build(self.dir, self.cfg)
        results = exp.query(self.dir, self.cfg)
        self.assertTrue(all(r["candidates"] for r in results))
        versions = {r["lib_version"] for r in results}
        self.assertEqual(len(versions), 1)
        top = results[0]["candidates"][0]
        self.assertEqual(top["sample_id"], "sample-000")
        self.assertIn("confidence", top)

    def test_align_mismatch(self):
        import copy
        exp.build(self.dir, self.cfg)
        cfg8 = copy.deepcopy(self.cfg)
        cfg8["fingerprint"]["time_quant"] = 8
        q = self.gt()["queries"][0]
        sig, rate = read_wav(self.dir / q["path"])
        rec, stats = fingerprint_signal(sig, rate, cfg8["fingerprint"])
        with self.assertRaises(AlignMismatch):
            search(Library(self.dir / "lib"), rec, q["query_id"], rate,
                   stats["n_frames"], cfg8["fingerprint"], cfg8["search"])


class TestReproducibility(Base):
    def test_byte_identical_pipeline(self):
        hashes = []
        for name in ("r1", "r2"):
            w = self.dir / name
            exp.generate(w, SEED, n_samples=6, n_queries=2)
            exp.baseline_hashes(w, self.cfg)
            exp.build(w, self.cfg)
            exp.query(w, self.cfg)
            import hashlib
            h = hashlib.sha256()
            for p in sorted(w.rglob("*")):
                if p.is_file():
                    h.update(str(p.relative_to(w)).encode())
                    h.update(p.read_bytes())
            hashes.append(h.hexdigest())
        self.assertEqual(hashes[0], hashes[1])


if __name__ == "__main__":
    unittest.main()
