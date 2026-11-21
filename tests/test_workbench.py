from __future__ import annotations

import dataclasses
import json
import os
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

from loudness_archive.archive import (
    ArchiveService,
    CancellationToken,
    manifest_file,
    segment_file,
    validate_manifest_chain,
)
from loudness_archive.config import load_config
from loudness_archive.errors import (
    E_BUSY,
    E_CANCELLED,
    E_MEMORY_BUDGET,
    E_META_INVALID,
    E_RECOVERY_INVALID,
    E_SEGMENT_MISMATCH,
    E_VERSION_LOSS,
    E_VERSION_STALE,
    E_WAV_INVALID,
    AppError,
)
from loudness_archive.generator import SimulatedCrash, generate_bundle
from loudness_archive.loudness import (
    default_downmix_weights,
    validate_downmix_weights,
)
from loudness_archive.metadata import (
    parse_metadata,
    refresh_checksum,
)
from loudness_archive.publish import FileVersionPublisher, InMemoryVersionPublisher
from loudness_archive.review import review_versions
from loudness_archive.scheduler import TaskScheduler
from loudness_archive.trace import TraceLogger
from loudness_archive.util import (
    atomic_write_json,
    canonical_json_bytes,
    hash_without,
    read_json,
)
from loudness_archive.versions import downgrade_document, upgrade_to_v3
from loudness_archive.wav import read_wav

ROOT = Path(__file__).resolve().parent.parent
CONFIG_PATH = ROOT / "config" / "workbench.json"


def _asset_paths(bundle: dict, out: str | Path, index: int = 0) -> tuple[Path, dict[int, Path]]:
    out = Path(out)
    asset = bundle["assets"][index]
    wav = out / "samples" / asset["wav"]
    metadata = {
        int(version.lower().lstrip("v")): out / path_data["path"]
        for version, path_data in asset["metadata"].items()
    }
    return wav, metadata


class CoreTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.config = load_config(CONFIG_PATH)
        cls.temp = tempfile.TemporaryDirectory(prefix="loudness-core-")
        cls.out = Path(cls.temp.name) / "bundle"
        cls.bundle = generate_bundle(cls.out, cls.config, seed=12345)
        cls.wav_path, cls.metadata_paths = _asset_paths(cls.bundle, cls.out, 0)
        cls.wav = read_wav(cls.wav_path)
        cls.documents = {
            version: parse_metadata(read_json(path), wav=cls.wav)
            for version, path in cls.metadata_paths.items()
        }

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_wav_variants_and_header_validation(self) -> None:
        layouts = set()
        bit_depths = set()
        for asset in self.bundle["assets"]:
            path = self.out / "samples" / asset["wav"]
            wav = read_wav(path)
            layouts.add(wav.channel_layout)
            bit_depths.add(wav.bits_per_sample)
            self.assertEqual(wav.sample_rate, 48000)
            self.assertEqual(wav.frames, 120000)
            self.assertEqual(wav.block_align, wav.channels * (wav.bits_per_sample // 8))
            self.assertEqual(wav.data_size, wav.frames * wav.block_align)
        self.assertEqual(layouts, {"MONO", "5.1"})
        self.assertEqual(bit_depths, {16, 24})

        surround = next(
            asset for asset in self.bundle["assets"] if asset["channel_layout"] == "5.1"
        )
        bad_mask = Path(self.temp.name) / "bad-mask.wav"
        blob = bytearray((self.out / "samples" / surround["wav"]).read_bytes())
        struct.pack_into("<I", blob, 40, 0x3)
        bad_mask.write_bytes(blob)
        with self.assertRaises(AppError) as caught:
            read_wav(bad_mask)
        self.assertEqual(caught.exception.code, E_WAV_INVALID)
        self.assertEqual(caught.exception.field, "fmt.dwChannelMask")

        truncated = Path(self.temp.name) / "truncated.wav"
        truncated.write_bytes(self.wav_path.read_bytes()[:-3])
        with self.assertRaises(AppError) as caught:
            read_wav(truncated)
        self.assertEqual(caught.exception.code, E_WAV_INVALID)

    def test_three_version_parsing_and_first_error_field(self) -> None:
        v1 = self.documents[1]
        v2 = self.documents[2]
        v3 = self.documents[3]
        self.assertEqual((v1.segment_count, v2.segment_count, v3.segment_count), (0, 4, 5))
        self.assertEqual(v1.boundary_source, "derived_fixed_window:v1")
        self.assertEqual(v2.boundary_source, "derived_equal_count:v2")
        self.assertEqual(v3.boundary_source, "metadata.explicit_boundaries")
        self.assertTrue(v3.archive_checksum.startswith("sha256:"))

        missing = dict(v3.raw)
        missing.pop("asset_id")
        with self.assertRaises(AppError) as caught:
            parse_metadata(missing)
        self.assertEqual(caught.exception.code, E_META_INVALID)
        self.assertEqual(caught.exception.field, "asset_id")

        unknown = dict(v1.raw)
        unknown["version"] = 99
        with self.assertRaises(AppError) as caught:
            parse_metadata(unknown)
        self.assertEqual(caught.exception.code, E_META_INVALID)
        self.assertEqual(caught.exception.field, "version")

        bad_checksum = dict(v3.raw)
        bad_checksum["archive_checksum"] = "sha256:" + "0" * 64
        with self.assertRaises(AppError) as caught:
            parse_metadata(bad_checksum, wav=self.wav)
        self.assertEqual(caught.exception.field, "archive_checksum")

        gap = json.loads(json.dumps(v3.raw))
        gap["segments"][1]["start_sample"] += 1
        gap = refresh_checksum(gap)
        with self.assertRaises(AppError) as caught:
            parse_metadata(gap, wav=self.wav)
        self.assertEqual(caught.exception.field, "segments[1].start_sample")

        wrong_duration = json.loads(json.dumps(v1.raw))
        wrong_duration["duration_samples"] += 1
        with self.assertRaises(AppError) as caught:
            parse_metadata(wrong_duration, wav=self.wav)
        self.assertEqual(caught.exception.field, "duration_samples")

    def test_upgrade_boundaries_and_auditable_mapping(self) -> None:
        v1_upgrade = upgrade_to_v3(self.documents[1], self.wav, self.config)
        self.assertEqual(v1_upgrade.derived_boundaries.source, "derived_fixed_window:v1")
        self.assertEqual(v1_upgrade.document.version, 3)
        self.assertEqual(v1_upgrade.document.segment_count, 5)
        fields = {entry["field"] for entry in v1_upgrade.field_mapping}
        self.assertIn("channel_layout", fields)
        self.assertIn("downmix_weights", fields)
        self.assertIn("archive_checksum", fields)
        self.assertIn("segments[].start_sample,end_sample", fields)
        checksum_decision = next(
            item for item in v1_upgrade.field_mapping if item["field"] == "archive_checksum"
        )
        self.assertEqual(checksum_decision["action"], "generated")
        self.assertIn("sha256", checksum_decision["algorithm"])
        self.assertEqual(
            v1_upgrade.payload["archive_checksum"],
            hash_without(v1_upgrade.payload, "archive_checksum"),
        )

        v2_upgrade = upgrade_to_v3(self.documents[2], self.wav, self.config)
        self.assertEqual(v2_upgrade.derived_boundaries.source, "derived_equal_count:v2")
        self.assertGreater(v2_upgrade.boundary_diff.affected_segment_count, 0)
        superseded = [
            entry
            for entry in v2_upgrade.field_mapping
            if "superseded_reason" in entry
        ]
        self.assertTrue(superseded)

        v3_upgrade = upgrade_to_v3(self.documents[3], self.wav, self.config)
        self.assertEqual(v3_upgrade.source_version, 3)
        self.assertEqual(v3_upgrade.payload["archive_checksum"], self.documents[3].archive_checksum)

    def test_downgrade_loss_manifest_and_reparse(self) -> None:
        upgraded = upgrade_to_v3(self.documents[3], self.wav, self.config)
        with self.assertRaises(AppError) as caught:
            downgrade_document(upgraded.document, 1, self.wav, self.config)
        self.assertEqual(caught.exception.code, E_VERSION_LOSS)
        losses = caught.exception.details["losses"]
        self.assertEqual([row["segment_index"] for row in losses], [0, 1, 2, 3, 4])
        self.assertIn("true_peak_dbtp", losses[0]["fields"])

        v1 = downgrade_document(
            upgraded.document,
            1,
            self.wav,
            self.config,
            allow_loss=True,
        )
        parsed_v1 = parse_metadata(v1.payload, wav=self.wav)
        self.assertEqual(parsed_v1.version, 1)
        self.assertTrue(v1.loss_manifest["mapping_table_independently_reproducible"])
        self.assertTrue(any(item["action"] == "dropped" for item in v1.field_mapping))

        v2 = downgrade_document(
            upgraded.document,
            2,
            self.wav,
            self.config,
            allow_loss=True,
        )
        parsed_v2 = parse_metadata(v2.payload, wav=self.wav)
        self.assertEqual(parsed_v2.version, 2)
        self.assertEqual(parsed_v2.segment_count, 5)
        boundary_loss = [
            item
            for item in v2.loss_manifest["losses"]
            if "start_sample" in item["fields"]
        ]
        self.assertEqual(len(boundary_loss), 5)

    def test_cross_version_review_is_reproducible(self) -> None:
        report = review_versions(self.wav, self.documents, self.config)
        self.assertFalse(report["ok"])
        self.assertEqual(report["code"], E_SEGMENT_MISMATCH)
        self.assertIsNotNone(report["first_mismatch"])
        self.assertIn(report["first_mismatch"]["field"], {"start_sample", "end_sample"})
        self.assertEqual(report["quantization_tolerance_db"], 0.1)
        self.assertGreaterEqual(len(report["comparisons"]), 3)
        for comparison in report["comparisons"]:
            self.assertIn("segment_diffs", comparison)
            self.assertIn("over_limit_sets", comparison)
        self.assertTrue(
            any(
                row["segment_index"] == 1
                for row in report["declared_vs_computed_mismatches"]
            )
        )
        first = canonical_json_bytes(report)
        second = canonical_json_bytes(review_versions(self.wav, self.documents, self.config))
        self.assertEqual(first, second)
        reproduction = report["reproduction"]
        self.assertIn("true_peak_formula", reproduction)
        self.assertEqual(len(reproduction["segment_data"]["v3"]), 5)
        for row in reproduction["segment_data"]["v3"]:
            self.assertIn("computed", row)
            self.assertIn("effective", row)

    def test_downmix_weight_validation(self) -> None:
        names = self.wav.channel_names
        weights = default_downmix_weights(self.wav.channel_layout, names)
        self.assertEqual(validate_downmix_weights(weights, names), weights)
        broken = list(weights)
        broken[0] = (broken[0][0], -1.0)
        with self.assertRaises(AppError) as caught:
            validate_downmix_weights(broken, names)
        self.assertEqual(caught.exception.field, "downmix_weights[0].weight")


class SchedulerTests(unittest.TestCase):
    def test_stable_order_memory_wait_and_busy(self) -> None:
        base = load_config(CONFIG_PATH)
        config = dataclasses.replace(
            base,
            max_parallel=2,
            per_task_memory_bytes=10,
            global_memory_bytes=10,
        )
        temp = tempfile.TemporaryDirectory(prefix="loudness-sched-")
        self.addCleanup(temp.cleanup)
        trace = TraceLogger(Path(temp.name) / "trace.jsonl")
        scheduler = TaskScheduler(config, trace)
        starts: list[str] = []
        lock = threading.Lock()

        def work(name: str) -> str:
            with lock:
                starts.append(name)
            time.sleep(0.02)
            return name

        scheduler.submit("asset-A", lambda: work("A"), 6)
        scheduler.submit("asset-B", lambda: work("B"), 6)
        with self.assertRaises(AppError) as caught:
            scheduler.submit("asset-A", lambda: work("duplicate"), 1)
        self.assertEqual(caught.exception.code, E_BUSY)
        with self.assertRaises(AppError) as caught:
            scheduler.submit("asset-C", lambda: work("C"), 11)
        self.assertEqual(caught.exception.code, E_MEMORY_BUDGET)

        results = scheduler.run_all()
        self.assertEqual(starts, ["A", "B"])
        self.assertEqual(results["task-000000"], "A")
        self.assertEqual(results["task-000001"], "B")
        events = trace.events()
        self.assertTrue(any(event["event"] == "task_wait_memory" for event in events))
        wait_event = next(event for event in events if event["event"] == "task_wait_memory")
        self.assertEqual(wait_event["asset_id"], "asset-B")
        self.assertEqual(wait_event["global_memory_bytes"], 10)


class ArchiveTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.config = load_config(CONFIG_PATH)
        cls.temp = tempfile.TemporaryDirectory(prefix="loudness-archive-")
        cls.bundle_dir = Path(cls.temp.name) / "bundle"
        cls.bundle = generate_bundle(cls.bundle_dir, cls.config, seed=99)
        cls.wav_path, cls.metadata_paths = _asset_paths(cls.bundle, cls.bundle_dir, 1)
        cls.wav = read_wav(cls.wav_path)
        cls.metadata = parse_metadata(read_json(cls.metadata_paths[3]), wav=cls.wav)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _fresh_workspace(self, name: str) -> Path:
        path = Path(self.temp.name) / name
        return path

    def test_crash_recovery_skips_committed_segments(self) -> None:
        workspace = self._fresh_workspace("recover")
        service = ArchiveService(workspace, self.config)

        def crash() -> None:
            raise SimulatedCrash("stop")

        with self.assertRaises(SimulatedCrash):
            service.execute(
                self.wav,
                self.metadata,
                target_version=3,
                crash_after_commits=2,
                crash_callback=crash,
            )
        manifest_path = manifest_file(workspace, self.metadata.asset_id)
        manifest = read_json(manifest_path)
        self.assertEqual([entry["index"] for entry in manifest["segments"]], [0, 1])
        self.assertEqual(manifest["manifest_revision"], 2)
        inodes = {
            index: os.stat(segment_file(workspace, self.metadata.asset_id, index)).st_ino
            for index in (0, 1)
        }

        recovered = service.execute(self.wav, self.metadata, target_version=3)
        self.assertEqual(recovered.recovery["committed_before"], [0, 1])
        self.assertEqual(recovered.recovery["computed_uncommitted"], [2, 3, 4])
        self.assertEqual(
            [row["segment_index"] for row in recovered.recovery["skipped_committed"]],
            [0, 1],
        )
        self.assertEqual(recovered.manifest["manifest_revision"], 5)
        for index, before in inodes.items():
            after = os.stat(segment_file(workspace, self.metadata.asset_id, index)).st_ino
            self.assertEqual(before, after, "committed segment file was rewritten")
        entries = validate_manifest_chain(workspace, read_json(manifest_path))
        self.assertEqual(len(entries), 5)
        events = TraceLogger(workspace / "trace.jsonl").events()
        self.assertTrue(any(event["event"] == "recover_skip" for event in events))
        self.assertTrue(any(event["event"] == "manifest_update" for event in events))

    def test_chain_break_and_truncation_locate_first_segment(self) -> None:
        workspace = self._fresh_workspace("broken-chain")
        service = ArchiveService(workspace, self.config)
        service.execute(self.wav, self.metadata, target_version=3)
        manifest_path = manifest_file(workspace, self.metadata.asset_id)
        manifest = read_json(manifest_path)
        manifest["segments"][1]["chain_hash"] = "sha256:" + "f" * 64
        manifest["manifest_sha256"] = hash_without(manifest, "manifest_sha256")
        atomic_write_json(manifest_path, manifest)
        with self.assertRaises(AppError) as caught:
            service.execute(self.wav, self.metadata, target_version=3)
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(caught.exception.field, "segments[1].chain_hash")
        self.assertEqual(caught.exception.details["segment_index"], 1)

        workspace = self._fresh_workspace("truncated")
        service = ArchiveService(workspace, self.config)
        service.execute(self.wav, self.metadata, target_version=3)
        target = segment_file(workspace, self.metadata.asset_id, 3)
        blob = target.read_bytes()
        target.write_bytes(blob[: len(blob) // 2])
        with self.assertRaises(AppError) as caught:
            service.execute(self.wav, self.metadata, target_version=3)
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(caught.exception.field, "segments[3].file")
        self.assertEqual(caught.exception.details["segment_index"], 3)

    def test_cooperative_cancel_before_commit(self) -> None:
        workspace = self._fresh_workspace("cancelled")
        service = ArchiveService(workspace, self.config)
        token = CancellationToken()
        token.cancel()
        with self.assertRaises(AppError) as caught:
            service.execute(
                self.wav,
                self.metadata,
                target_version=3,
                cancel_token=token,
            )
        self.assertEqual(caught.exception.code, E_CANCELLED)
        self.assertFalse(manifest_file(workspace, self.metadata.asset_id).exists())

    def test_strong_kill_via_cli_then_recover(self) -> None:
        workspace = self._fresh_workspace("strong-kill")
        base = [
            sys.executable,
            "-m",
            "loudness_archive",
            "--config",
            str(CONFIG_PATH),
        ]
        crash = subprocess.run(
            base
            + [
                "archive",
                "--workspace",
                str(workspace),
                "--wav",
                str(self.wav_path),
                "--metadata",
                str(self.metadata_paths[3]),
                "--crash-after",
                "2",
                "--no-publish",
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        self.assertEqual(crash.returncode, 137, crash.stderr)
        manifest = read_json(manifest_file(workspace, self.metadata.asset_id))
        self.assertEqual([entry["index"] for entry in manifest["segments"]], [0, 1])
        recover = subprocess.run(
            base
            + [
                "recover",
                "--workspace",
                str(workspace),
                "--wav",
                str(self.wav_path),
                "--metadata",
                str(self.metadata_paths[3]),
                "--no-publish",
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        self.assertEqual(recover.returncode, 0, recover.stderr)
        payload = json.loads(recover.stdout)
        self.assertEqual(payload["recovery"]["committed_before"], [0, 1])
        self.assertEqual(payload["recovery"]["computed_uncommitted"], [2, 3, 4])

    def test_file_version_publication_query_and_stale(self) -> None:
        workspace = self._fresh_workspace("publish")
        service = ArchiveService(workspace, self.config)
        result = service.execute(self.wav, self.metadata, target_version=3)
        publisher = FileVersionPublisher(workspace)
        published = publisher.publish(
            result.manifest,
            [analysis.to_dict() for analysis in result.analyses],
            target_version=3,
            loss_manifest=None,
            field_mapping=list(result.field_mapping),
            expected_manifest_chain_head=result.manifest["chain_head"],
        )
        queried = publisher.query(
            published["version_id"],
            expected_version_hash=published["content_hash"],
        )
        self.assertEqual(queried["hit_version"], published["version_id"])
        self.assertEqual(queried["content_hash"], published["content_hash"])
        self.assertEqual(len(queried["data"]["segments"]), 5)
        with self.assertRaises(AppError) as caught:
            publisher.query(
                published["version_id"],
                expected_version_hash="sha256:" + "0" * 64,
            )
        self.assertEqual(caught.exception.code, E_VERSION_STALE)

        manifest_path = manifest_file(workspace, self.metadata.asset_id)
        manifest = read_json(manifest_path)
        manifest["chain_head"] = "sha256:" + "9" * 64
        manifest["manifest_sha256"] = hash_without(manifest, "manifest_sha256")
        atomic_write_json(manifest_path, manifest)
        with self.assertRaises(AppError) as caught:
            publisher.query(published["version_id"])
        self.assertEqual(caught.exception.code, E_VERSION_STALE)


class PublicationIsolationTests(unittest.TestCase):
    def test_old_snapshot_survives_stale_publish_attempt(self) -> None:
        publisher = InMemoryVersionPublisher()
        body1 = {
            "asset_id": "asset",
            "manifest_chain_head": "sha256:" + "1" * 64,
            "manifest_revision": 1,
            "segments": [{"index": 0, "marker": "old"}],
        }
        first = publisher.publish(body1, expected_manifest_chain_head=body1["manifest_chain_head"])
        with self.assertRaises(AppError) as caught:
            publisher.publish(
                {
                    **body1,
                    "manifest_chain_head": "sha256:" + "2" * 64,
                    "manifest_revision": 2,
                },
                expected_manifest_chain_head="sha256:" + "3" * 64,
            )
        self.assertEqual(caught.exception.code, E_VERSION_STALE)
        old = publisher.query(first.version_id)
        self.assertEqual(old["data"]["segments"][0]["marker"], "old")

    def test_concurrent_query_never_observes_half_snapshot(self) -> None:
        publisher = InMemoryVersionPublisher()
        stop = threading.Event()
        errors: list[BaseException] = []

        def reader() -> None:
            while not stop.is_set():
                try:
                    result = publisher.query()
                    data = result["data"]
                    marker = data["marker"]
                    self.assertEqual(data["segments"], [{"index": index, "marker": marker} for index in range(80)])
                except AppError as exc:
                    if exc.code != E_VERSION_STALE:
                        errors.append(exc)
                except Exception as exc:  # noqa: BLE001 - collect reader failures
                    errors.append(exc)

        threads = [threading.Thread(target=reader) for _ in range(4)]
        for thread in threads:
            thread.start()
        try:
            for marker in range(30):
                publisher.publish_body(
                    {
                        "asset_id": "concurrent",
                        "manifest_chain_head": f"sha256:{marker:064x}",
                        "manifest_revision": marker,
                        "marker": marker,
                        "segments": [
                            {"index": index, "marker": marker} for index in range(80)
                        ],
                    }
                )
                time.sleep(0.001)
        finally:
            stop.set()
            for thread in threads:
                thread.join(timeout=5)
        self.assertEqual(errors, [])


class DeterminismTests(unittest.TestCase):
    def test_same_seed_and_operations_produce_identical_files(self) -> None:
        config = load_config(CONFIG_PATH)
        temp = tempfile.TemporaryDirectory(prefix="loudness-determinism-")
        self.addCleanup(temp.cleanup)
        first = Path(temp.name) / "one"
        second = Path(temp.name) / "two"
        generate_bundle(first, config, seed=777)
        generate_bundle(second, config, seed=777)
        first_files = sorted(path.relative_to(first) for path in first.rglob("*") if path.is_file())
        second_files = sorted(
            path.relative_to(second) for path in second.rglob("*") if path.is_file()
        )
        self.assertEqual(first_files, second_files)
        for relative in first_files:
            self.assertEqual(
                (first / relative).read_bytes(),
                (second / relative).read_bytes(),
                f"non-deterministic artifact: {relative}",
            )

        trace_lines = (first / "trace.jsonl").read_text(encoding="utf-8").splitlines()
        trace = [json.loads(line) for line in trace_lines]
        events = {entry["event"] for entry in trace}
        self.assertIn("baseline", events)
        self.assertIn("segment_commit", events)
        self.assertIn("recover_skip", events)
        self.assertIn("publish", events)
        baseline_event = next(entry for entry in trace if entry["event"] == "baseline")
        self.assertIn("metadata_version", baseline_event)
        self.assertIn("segment_index", baseline_event)
        self.assertIn("over_limit", baseline_event)
        self.assertIn("hash", baseline_event)


if __name__ == "__main__":
    unittest.main()

