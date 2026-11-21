from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from tileforge.binary import atomic_write_bytes, parse_sequence, write_sequence_atomic
from tileforge.config import JobConfig, MemoryConfig
from tileforge.errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CONVERSION_MISMATCH,
    E_PREVIEW_MISMATCH,
    E_VERSION_STALE,
    TileForgeError,
)
from tileforge.locks import RunLock
from tileforge.pipeline import convert_incremental, offline_convert
from tileforge.preview import PreviewCursor, load_offline_report, preview_frame
from tileforge.samplegen import SAMPLE_SPECS, generate_sequence, sample_config
from tileforge.store import VersionStore


class PipelineTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.sequence = generate_sequence(SAMPLE_SPECS[0], 73)
        self.input_path = self.root / "input.tcseq"
        self.input_hash = write_sequence_atomic(self.input_path, self.sequence)
        self.config = sample_config(SAMPLE_SPECS[0])
        self.workspace = self.root / "workbench"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_incremental_publish_offline_preview_reuse_and_rollback(self) -> None:
        first = convert_incremental(self.input_path, self.config, self.workspace)
        self.assertEqual(first.affected_tiles, list(range(12)))
        self.assertEqual(first.version_id, 1)
        current = VersionStore(self.workspace).load_current()
        self.assertEqual(current.version_hash, first.version_hash)

        changed = replace(self.config, block_exposure_stops={3: 0.25})
        second = convert_incremental(
            self.input_path,
            changed,
            self.workspace,
            expected_base_hash=first.version_hash,
        )
        self.assertEqual(second.affected_tiles, [3])
        self.assertEqual(len(second.reused_tiles), 11)
        self.assertEqual(second.version_id, 2)
        manifest = VersionStore(self.workspace).load_current().manifest
        reused = [record for record in manifest["tile_records"] if record["reused"]]
        recomputed = [record for record in manifest["tile_records"] if not record["reused"]]
        self.assertEqual(len(reused), 11)
        self.assertEqual([record["tile_id"] for record in recomputed], [3])
        self.assertEqual(reused[0]["origin_version"], 1)

        offline_path = self.root / "offline.tcseq"
        offline = offline_convert(self.input_path, offline_path, changed)
        self.assertEqual(offline.version_hash, second.version_hash)
        cursor = PreviewCursor(self.workspace, expected_version_hash=second.version_hash)
        cursor.refresh()
        cursor.advance_to_end()
        comparison = cursor.compare_offline(load_offline_report(offline_path.with_suffix(".tcseq.report.json")))
        self.assertTrue(comparison["ok"])
        self.assertEqual(comparison["compared_tiles"], 12)

        store = VersionStore(self.workspace)
        rolled = store.rollback(1, expected_current_hash=second.version_hash)
        self.assertEqual(rolled["version_id"], 1)
        self.assertEqual(preview_frame(self.workspace)["version_id"], 1)
        with self.assertRaises(TileForgeError) as context:
            PreviewCursor(self.workspace, expected_version_hash=second.version_hash).refresh()
        self.assertEqual(context.exception.code, E_VERSION_STALE)

    def test_preview_mismatch_locates_first_tile_and_field(self) -> None:
        summary = convert_incremental(self.input_path, self.config, self.workspace)
        offline_path = self.root / "offline.tcseq"
        offline_convert(self.input_path, offline_path, self.config)
        report_path = offline_path.with_suffix(".tcseq.report.json")
        report = json.loads(report_path.read_text(encoding="utf-8"))
        report["tiles"][0]["content_hash"] = "deliberately-wrong"
        payload = bytearray(__import__("base64").b64decode(report["tiles"][0]["payload_b64"]))
        payload[0] = (payload[0] + 7) & 0xFF
        report["tiles"][0]["payload_b64"] = __import__("base64").b64encode(bytes(payload)).decode("ascii")
        cursor = PreviewCursor(self.workspace, expected_version_hash=summary.version_hash)
        cursor.refresh()
        cursor.advance_to_end()
        with self.assertRaises(TileForgeError) as context:
            cursor.compare_offline(report)
        exc = context.exception
        self.assertEqual(exc.code, E_PREVIEW_MISMATCH)
        self.assertEqual(exc.details["tile_id"], 0)
        self.assertEqual(exc.details["field"], "sample_value")
        self.assertEqual(exc.details["offset"], 0)

    def test_preview_rejects_incomplete_offline_tile_coverage(self) -> None:
        summary = convert_incremental(self.input_path, self.config, self.workspace)
        offline_path = self.root / "offline.tcseq"
        offline_convert(self.input_path, offline_path, self.config)
        report = load_offline_report(offline_path.with_suffix(".tcseq.report.json"))
        report["tiles"] = report["tiles"][:-1]
        cursor = PreviewCursor(self.workspace, expected_version_hash=summary.version_hash)
        cursor.refresh()
        cursor.advance_to_end()
        with self.assertRaises(TileForgeError) as context:
            cursor.compare_offline(report)
        self.assertEqual(context.exception.code, E_PREVIEW_MISMATCH)
        self.assertEqual(context.exception.details["missing"], [11])

    def test_budget_rejection_preserves_previous_version(self) -> None:
        first = convert_incremental(self.input_path, self.config, self.workspace)
        low_memory = MemoryConfig(
            parallelism=2,
            tile_budget_bytes=1000,
            run_budget_bytes=2000,
            downgrade_profiles=("full", "banded", "streaming"),
            estimator_overhead_bytes=12_288,
        )
        rejected = replace(self.config, tone_map_curve="aces", memory=low_memory)
        with self.assertRaises(TileForgeError) as context:
            convert_incremental(self.input_path, rejected, self.workspace)
        self.assertEqual(context.exception.code, E_BUDGET_EXCEEDED)
        current = VersionStore(self.workspace).load_current()
        self.assertEqual(current.version_hash, first.version_hash)
        self.assertEqual(len(VersionStore(self.workspace).list_versions()), 1)
        self.assertEqual(list((self.workspace / "runs").iterdir()), [])

    def test_declared_downgrade_records_stage_and_usage(self) -> None:
        memory = MemoryConfig(
            parallelism=2,
            tile_budget_bytes=30_000,
            run_budget_bytes=60_000,
            downgrade_profiles=("full", "banded", "streaming"),
            estimator_overhead_bytes=12_288,
        )
        sequence = generate_sequence(SAMPLE_SPECS[2], 74)
        input_path = self.root / "twelve.tcseq"
        write_sequence_atomic(input_path, sequence)
        config = replace(sample_config(SAMPLE_SPECS[2]), memory=memory)
        summary = convert_incremental(input_path, config, self.workspace)
        self.assertTrue(all(stage == "banded" for stage in summary.downgrade_stages.values()))
        trace_lines = sorted((self.workspace / "traces").glob("*.jsonl"))
        events = [json.loads(line) for path in trace_lines for line in path.read_text(encoding="utf-8").splitlines()]
        accepted = [event for event in events if event["event"] == "downgrade_accepted"]
        self.assertTrue(accepted)
        self.assertFalse(any(event.get("precision_changed") for event in events if event["event"] == "downgrade_accepted"))
        self.assertTrue(all(event["usage_bytes"] <= 30_000 for event in accepted))

    def test_same_sequence_lock_returns_busy(self) -> None:
        input_hash = parse_sequence(self.input_path).file_sha256
        lock_path = self.workspace / "locks" / f"run-{input_hash[:32]}.lock"
        with RunLock(lock_path, input_hash):
            with self.assertRaises(TileForgeError) as context:
                convert_incremental(self.input_path, self.config, self.workspace)
        self.assertEqual(context.exception.code, E_BUSY)
        with self.assertRaises(TileForgeError) as context:
            convert_incremental(
                self.input_path,
                self.config,
                self.workspace,
                expected_base_hash="0" * 64,
            )
        self.assertEqual(context.exception.code, E_VERSION_STALE)

    def test_declared_conversion_identifier_is_validated(self) -> None:
        mismatched = replace(
            self.config,
            expected_conversion_id=self.sequence.header.conversion_id ^ 0xFFFFFFFF,
        )
        with self.assertRaises(TileForgeError) as context:
            mismatched.validate_for_sequence(
                self.sequence.header.width,
                self.sequence.header.height,
                self.sequence.header.block_count,
                self.sequence.header.source_transfer,
                self.sequence.header.conversion_id,
            )
        self.assertEqual(context.exception.code, E_CONVERSION_MISMATCH)


if __name__ == "__main__":
    unittest.main()
