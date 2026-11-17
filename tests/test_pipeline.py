"""End-to-end deterministic DSP, mastering and resume guarantees."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
import unittest

import numpy as np
import soundfile as sf

from stage_noise.generator import generate
from stage_noise.manifest import load_manifest
from stage_noise.persistence.snapshot import SnapshotStore
from stage_noise.pipeline import PipelineInterrupted, run_pipeline
from stage_noise.reporting.reports import build_report, write_report

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEMO = os.path.join(ROOT, "configs", "demo_manifest.json")


def _shaudio(path):
    with open(path, "rb") as handle:
        return handle.read()


def _scenario():
    base = tempfile.mkdtemp(prefix="sn_e2e_")
    target = os.path.join(base, "configs")
    shutil.copytree(os.path.join(ROOT, "configs"), target)
    manifest = load_manifest(os.path.join(target, "demo_manifest.json"))
    return base, target, manifest


def _render(manifest, state=None):
    base = manifest.source_dir
    outputs = {
        "master": os.path.join(base, "master_stereo.wav"),
        "report": os.path.join(base, "noise_report.json"),
        "recovery": os.path.join(base, "recovery_report.json"),
    }
    state = state or os.path.join(base, ".render_state")
    return run_pipeline(manifest, state, outputs)


class EndToEndTests(unittest.TestCase):
    def setUp(self):
        self.base, self.target, self.manifest = _scenario()
        generate(self.manifest)
        with open(os.path.join(self.target, "injection_truth.json"),
                  encoding="utf-8") as handle:
            truth_doc = json.load(handle)
        self.truth = {
            item["mic_name"]: {inj["segment_id"]: inj
                               for inj in item["injections"]}
            for item in truth_doc["microphones"]
        }
        self.originals = {
            mic.name: sf.read(os.path.join(self.target, mic.path),
                              dtype="float64")[0]
            for mic in self.manifest.mics
        }

    def tearDown(self):
        shutil.rmtree(self.base, ignore_errors=True)

    def test_generation_is_byte_identical(self):
        hashes_before = {
            mic.name: hashlib.sha256(_shaudio(
                os.path.join(self.target, mic.path))).hexdigest()
            for mic in self.manifest.mics
        }
        generate(self.manifest)
        for mic in self.manifest.mics:
            digest = hashlib.sha256(_shaudio(
                os.path.join(self.target, mic.path))).hexdigest()
            self.assertEqual(digest, hashes_before[mic.name])

    def test_tracing_matches_injection_truth(self):
        result = _render(self.manifest)
        for mic_trace in result["trace_results"]:
            name = mic_trace["mic_name"]
            for entry in mic_trace["entries"]:
                truth = self.truth[name].get(entry["segment_id"])
                if truth is None:
                    self.assertFalse(entry["detected"])
                    continue
                self.assertTrue(entry["detected"])
                self.assertLessEqual(
                    abs(entry["center_frequency_hz"]
                        - truth["center_frequency_hz"]),
                    self.manifest.center_frequency_tolerance_hz)
                self.assertLessEqual(
                    abs(entry["start_time"] - truth["start_time"]),
                    self.manifest.timing_tolerance_ms / 1000.0)
                self.assertLessEqual(
                    abs(entry["end_time"] - truth["end_time"]),
                    self.manifest.timing_tolerance_ms / 1000.0)
        total_false = sum(item["false_positive_segments"]
                          for item in result["trace_results"])
        self.assertLessEqual(
            total_false, self.manifest.max_false_action_segments)

    def test_denoise_attenuation_and_content_protection(self):
        result = _render(self.manifest)
        detected = {
            (entry["mic_name"], entry["segment_id"])
            for item in result["trace_results"]
            for entry in item["entries"] if entry["detected"]
        }
        for report in result["denoise_reports"]:
            self.assertLessEqual(
                abs(report["content_band"]["content_loss_db"]),
                self.manifest.max_content_loss_db)
            for segment in report["segment_metrics"]:
                key = (report["mic_name"], segment["segment_id"])
                if key in detected:
                    self.assertGreaterEqual(
                        segment["band_attenuation_db"],
                        self.manifest.min_attenuation_db)

    def test_sample_count_and_outside_interval_untouched(self):
        result = _render(self.manifest)
        margin = int(round(self.manifest.injection_margin_ms / 1000.0
                           * self.manifest.sample_rate))
        for mic in self.manifest.mics:
            processed = sf.read(os.path.join(self.target, mic.path),
                                dtype="float64")[0]
            self.assertEqual(len(processed), len(self.originals[mic.name]))
            protected = np.ones(len(processed), dtype=bool)
            for action in self.manifest.actions:
                start = max(0, action.start_sample - margin)
                end = min(len(processed), action.end_sample + margin)
                protected[start:end] = False
            np.testing.assert_array_equal(processed[protected],
                                          self.originals[mic.name][protected])
        self.assertEqual(result["total_samples"],
                         self.manifest.total_samples)

    def test_master_loudness_and_true_peak(self):
        result = _render(self.manifest)
        mastering = result["master_report"]
        self.assertLessEqual(
            abs(mastering["final_integrated_loudness_lufs"]
                - self.manifest.target_loudness_lufs), 0.5)
        self.assertLessEqual(
            mastering["final_true_peak_dbtp"],
            self.manifest.true_peak_limit_dbtp)
        master, sr = sf.read(result["master_path"], always_2d=True,
                             dtype="float64")
        self.assertEqual(master.shape,
                         (self.manifest.total_samples, 2))
        self.assertEqual(sr, self.manifest.sample_rate)

    def test_resume_produces_byte_identical_master(self):
        full = _render(self.manifest)
        full_bytes = _shaudio(full["master_path"])

        # Fresh scenario, stop after 7 chunks, then resume.
        base2, target2, manifest2 = _scenario()
        self.addCleanup(shutil.rmtree, base2, True)
        generate(manifest2)
        state = os.path.join(target2, ".render_state")
        with self.assertRaises(PipelineInterrupted):
            run_pipeline(
                manifest2, state,
                {"master": os.path.join(target2, "master_stereo.wav"),
                 "report": os.path.join(target2, "noise_report.json"),
                 "recovery": os.path.join(target2, "recovery.json")},
                interrupt_after_chunk=7)
        store = SnapshotStore(state)
        self.assertEqual(store.load()["completed_chunks"], 8)
        resumed = run_pipeline(
            manifest2, state,
            {"master": os.path.join(target2, "master_stereo.wav"),
             "report": os.path.join(target2, "noise_report.json"),
             "recovery": os.path.join(target2, "recovery.json")})
        self.assertEqual(_shaudio(resumed["master_path"]), full_bytes)

    def test_corrupt_snapshot_restarts_and_matches(self):
        full = _render(self.manifest)
        full_bytes = _shaudio(full["master_path"])

        base2, target2, manifest2 = _scenario()
        self.addCleanup(shutil.rmtree, base2, True)
        generate(manifest2)
        state = os.path.join(target2, ".render_state")
        outputs = {"master": os.path.join(target2, "master_stereo.wav"),
                   "report": os.path.join(target2, "noise_report.json"),
                   "recovery": os.path.join(target2, "recovery.json")}
        with self.assertRaises(PipelineInterrupted):
            run_pipeline(manifest2, state, outputs,
                         interrupt_after_chunk=4)
        with open(os.path.join(state, "snapshot.json"), "w",
                  encoding="utf-8") as handle:
            handle.write("{corrupted")
        resumed = run_pipeline(manifest2, state, outputs)
        self.assertEqual(_shaudio(resumed["master_path"]), full_bytes)

    def test_report_is_stable_and_recomputable(self):
        result = _render(self.manifest)
        report = build_report(self.manifest, result, 1.234)
        path = os.path.join(self.target, "stable_report.json")
        write_report(report, path)
        with open(path, encoding="utf-8") as handle:
            first = handle.read()
        write_report(report, path)
        with open(path, encoding="utf-8") as handle:
            self.assertEqual(first, handle.read())
        self.assertNotIn(os.path.abspath(self.target), first)
        # Recomputed residual energies must match the report figures.
        from stage_noise.processing.audioutil import (dbfs,
                                                      segment_band_energy)
        band = self.manifest.noise_band_hz
        for mic_block in report["residual_check_from_wav"]:
            mic = next(m for m in self.manifest.mics
                       if m.name == mic_block["mic_name"])
            signal, sr = sf.read(os.path.join(self.target, mic.path),
                                 dtype="float64")
            for entry in mic_block["segments"]:
                action = next(a for a in self.manifest.actions
                              if a.segment_id == entry["segment_id"])
                energy = dbfs(segment_band_energy(
                    signal, action.start_sample, action.end_sample,
                    band, sr))
                self.assertAlmostEqual(
                    energy, entry["noise_band_energy_dbfs"], places=2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
