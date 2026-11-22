from __future__ import annotations

import copy
import json
import tempfile
import threading
import time
import unittest
from pathlib import Path

from live_transcode.engine import TranscodeEngine
from live_transcode.errors import (
    E_BUSY,
    E_SIDECAR_DRIFT,
    E_VERSION_STALE,
    TaskError,
)
from live_transcode.models import TierParams, sha256_bytes
from live_transcode.sidecar import audit_tier

from .support import Workspace, file_sha


class EngineTests(unittest.TestCase):
    def setUp(self) -> None:
        self.ctx = Workspace("lt-engine-")
        self.ctx.__enter__()
        self.fixtures = self.ctx.fixtures(seed=81, duration=1.2)

    def tearDown(self) -> None:
        self.ctx.__exit__(None, None, None)

    def test_parameter_versions_are_monotonic_and_deterministic(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        first = engine.catalog.configure("t", 24000, 32, 16, 256)
        same = engine.catalog.configure("t", 24000, 32, 16, 256)
        changed = engine.catalog.configure("t", 24000, 48, 16, 256)
        changed_again = engine.catalog.configure("t", 24000, 48, 24, 256)
        self.assertEqual(first.param_version, 1)
        self.assertEqual(same.param_version, 1)
        self.assertEqual(changed.param_version, 2)
        self.assertEqual(changed_again.param_version, 3)

        wav = self.fixtures[0]["path"]
        a = engine.generate_tier(wav, first)
        b = engine.generate_tier(wav, first)
        self.assertEqual(a.tier_version_hash, b.tier_version_hash)
        self.assertEqual(
            [item["content_hash"] for item in a.sidecar["slices"]],
            [item["content_hash"] for item in b.sidecar["slices"]],
        )
        self.assertTrue(all(
            item["content_hash"] == sha256_bytes(
                engine.store.read_slice_audio(a.run_dir, item["index"])
            )
            for item in a.sidecar["slices"]
        ))

    def test_quantization_and_filter_changes_update_partial_audio(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[0]["path"]
        base = engine.generate_tier(
            wav, TierParams("t", 24000, 32, 16, 1, 512)
        )
        quant = engine.generate_tier(
            wav, TierParams("t", 24000, 32, 24, 2, 512)
        )
        self.assertEqual(quant.reused_slices, 0)
        self.assertEqual(quant.recomputed_slices, quant.sidecar["total_slices"])
        self.assertTrue(all(
            a["content_hash"] != b["content_hash"]
            for a, b in zip(base.sidecar["slices"], quant.sidecar["slices"])
        ))

        filtered = engine.generate_tier(
            wav, TierParams("t", 24000, 48, 24, 3, 512)
        )
        self.assertGreater(filtered.reused_audio_slices, 0)
        self.assertEqual(
            filtered.sidecar_recomputed_slices,
            filtered.sidecar["total_slices"],
        )
        trace = [
            json.loads(line)
            for line in filtered.trace_path.read_text(encoding="utf-8").splitlines()
        ]
        plans = [
            record
            for record in trace
            if record["action"] == "dependency_plan"
            and record["param_version"] == 3
        ]
        self.assertEqual(len(plans), 1)
        self.assertEqual(plans[0]["changed_fields"], ["filter_length"])
        self.assertTrue(plans[0]["phase_changed"])

    def test_phase_propagation_chain_is_logged(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[0]["path"]
        before = engine.generate_tier(
            wav, TierParams("t", 44100, 32, 16, 1, 256)
        )
        after = engine.generate_tier(
            wav, TierParams("t", 44100, 48, 16, 2, 256)
        )
        phase_before = [
            item["phase_remainder_num"] for item in before.sidecar["slices"]
        ]
        phase_after = [
            item["phase_remainder_num"] for item in after.sidecar["slices"]
        ]
        self.assertNotEqual(phase_before[0], phase_after[0])
        self.assertNotEqual(phase_before[10], phase_after[10])
        records = [
            json.loads(line)
            for line in after.trace_path.read_text(encoding="utf-8").splitlines()
        ]
        recomputed = [
            record
            for record in records
            if record["action"] == "slice_recomputed"
            and record["param_version"] == 2
        ]
        self.assertTrue(recomputed)
        self.assertEqual(recomputed[0]["reason"], "param:filter_length")
        self.assertEqual(recomputed[0]["propagation_chain"], [0])
        self.assertEqual(recomputed[1]["reason"], "phase_propagation")
        self.assertEqual(recomputed[1]["propagation_chain"], [0, 1])
        self.assertEqual(
            recomputed[-1]["propagation_chain"],
            list(range(len(recomputed))),
        )

    def test_unchanged_parameter_revision_reuses_all_slices(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[3]["path"]
        base = engine.generate_tier(
            wav, TierParams("t", 24000, 32, 24, 1, 256)
        )
        revision = engine.generate_tier(
            wav, TierParams("t", 24000, 32, 24, 2, 256)
        )
        self.assertEqual(revision.reused_slices, revision.sidecar["total_slices"])
        self.assertEqual(revision.sidecar_recomputed_slices, 0)
        self.assertEqual(
            revision.reused_audio_slices,
            revision.sidecar["total_slices"],
        )
        self.assertEqual(
            [item["content_hash"] for item in base.sidecar["slices"]],
            [item["content_hash"] for item in revision.sidecar["slices"]],
        )

    def test_sidecar_and_buffer_drift_attribution(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[0]["path"]
        old = engine.generate_tier(
            wav, TierParams("t", 24000, 32, 16, 1, 256)
        )
        new = engine.generate_tier(
            wav, TierParams("t", 24000, 32, 24, 2, 256)
        )
        info, source = __import__("live_transcode.wavio", fromlist=["load_wav"]).load_wav(wav)
        tier = TierParams(**new.sidecar["tier_params"])
        audio = [
            engine.store.read_slice_audio(new.run_dir, index)
            for index in range(new.sidecar["total_slices"])
        ]

        stale_sidecar = copy.deepcopy(new.sidecar)
        stale_sidecar["slices"][0] = copy.deepcopy(old.sidecar["slices"][0])
        with self.assertRaises(TaskError) as sidecar_error:
            audit_tier(source, info, tier, stale_sidecar, audio)
        self.assertEqual(sidecar_error.exception.code, E_SIDECAR_DRIFT)
        self.assertEqual(sidecar_error.exception.details["first_drift_slice"], 0)
        self.assertEqual(sidecar_error.exception.details["stale_side"], "sidecar")
        first = sidecar_error.exception.details["report"]["drifting_slices"][0]
        self.assertIn("content_hash", first["meta_fields"])

        stale_buffer = list(audio)
        stale_buffer[0] = engine.store.read_slice_audio(old.run_dir, 0)
        with self.assertRaises(TaskError) as buffer_error:
            audit_tier(source, info, tier, new.sidecar, stale_buffer)
        self.assertEqual(buffer_error.exception.code, E_SIDECAR_DRIFT)
        self.assertEqual(buffer_error.exception.details["stale_side"], "buffer")
        first = buffer_error.exception.details["report"]["drifting_slices"][0]
        self.assertGreater(first["deviation_bytes"], 0)
        self.assertGreater(first["deviation_samples"], 0)

    def test_publish_manifest_stale_and_query_consistency(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[2]["path"]
        first_tier = engine.catalog.configure("t", 24000, 32, 16, 256)
        engine.generate_tier(wav, first_tier)
        first = engine.store.query_tier("t")
        second_tier = engine.catalog.configure("t", 24000, 32, 24, 256)
        engine.generate_tier(wav, second_tier)
        second = engine.store.query_tier("t")
        self.assertNotEqual(first.generation_hash, second.generation_hash)
        self.assertEqual(second.param_version, 2)
        with self.assertRaises(TaskError) as raised:
            engine.store.query_tier("t", expected_manifest_hash=first.generation_hash)
        self.assertEqual(raised.exception.code, E_VERSION_STALE)
        for item, pcm in zip(second.sidecar["slices"], second.audio_slices):
            self.assertEqual(item["content_hash"], sha256_bytes(pcm))

    def test_query_isolation_during_replacement(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[2]["path"]
        engine.generate_tier(
            wav, engine.catalog.configure("t", 22050, 32, 16, 256)
        )
        errors: list[BaseException] = []
        observations: list[tuple[int, str, str]] = []
        stop = threading.Event()

        def writer() -> None:
            try:
                for quant in (24, 16, 24):
                    tier = engine.catalog.configure("t", 22050, 32, quant, 256)
                    engine.generate_tier(wav, tier)
            except BaseException as exc:
                errors.append(exc)
            finally:
                stop.set()

        def reader() -> None:
            try:
                while not stop.is_set():
                    published = engine.store.query_tier("t")
                    self.assertEqual(
                        published.param_version,
                        published.sidecar["param_version"],
                    )
                    for item, pcm in zip(
                        published.sidecar["slices"], published.audio_slices
                    ):
                        self.assertEqual(item["content_hash"], sha256_bytes(pcm))
                    observations.append(
                        (
                            published.generation,
                            published.generation_hash,
                            published.tier_version_hash,
                        )
                    )
            except BaseException as exc:
                errors.append(exc)

        writer_thread = threading.Thread(target=writer)
        reader_thread = threading.Thread(target=reader)
        writer_thread.start()
        reader_thread.start()
        writer_thread.join(30)
        reader_thread.join(30)
        self.assertFalse(errors, errors)
        self.assertTrue(observations)

    def test_same_tier_concurrent_start_returns_busy(self) -> None:
        wav = self.fixtures[3]["path"]
        engine_a = TranscodeEngine(self.ctx.store)
        engine_b = TranscodeEngine(self.ctx.store)
        tier = TierParams("busy", 22050, 256, 24, 1, 512)
        started = threading.Event()
        results: list[object] = []

        def work() -> None:
            try:
                results.append(
                    engine_a.generate_tier(
                        wav,
                        tier,
                        progress=lambda event: (
                            started.set() if event.get("event") == "slice" else None
                        ),
                    )
                )
            except BaseException as exc:
                results.append(exc)

        thread = threading.Thread(target=work)
        thread.start()
        self.assertTrue(started.wait(10))
        with self.assertRaises(TaskError) as raised:
            engine_b.generate_tier(wav, tier)
        self.assertEqual(raised.exception.code, E_BUSY)
        thread.join(30)
        self.assertEqual(len(results), 1)


if __name__ == "__main__":
    unittest.main()

