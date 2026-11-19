from __future__ import annotations

import hashlib
import json
import os
import shutil
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from backup_chain.common import (
    BC_DATABASE_PATH_INSIDE_REPO,
    BusinessError,
    atomic_write_json,
    stable_state_digest,
)
from backup_chain.service import BackupChainService
from generate_sample import write_chunk


class MiniRepository:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.repo = root / "repo"
        self.repo.mkdir(parents=True)
        self.manifest_path = self.repo / "manifest.json"
        self.block_ids = ["blk-0001", "blk-0002", "blk-0003"]
        self.sizes = [70000, 72000, 75000]
        self.current: dict[str, dict[str, object]] = {}
        self.full_blocks: list[dict[str, object]] = []
        self.increments: list[dict[str, object]] = []
        self._make()

    def _write(self, partition: str, backup_id: str, block_id: str, version: str, size: int) -> dict[str, object]:
        rel = f"{partition}/{backup_id}/{block_id}.chunk"
        return {
            **write_chunk(
                self.repo / rel,
                seed=7,
                backup_id=backup_id,
                block_id=block_id,
                version=version,
                size=size,
            ),
            "path": rel,
        }

    def _make(self) -> None:
        for index, block_id in enumerate(self.block_ids):
            primary = self._write(
                "p-primary", "full-000", block_id, "full-current", self.sizes[index]
            )
            if index == 1:
                legacy = self._write(
                    "p-legacy", "full-000", block_id, "full-legacy", self.sizes[index]
                )
            else:
                legacy_rel = f"p-legacy/full-000/{block_id}.chunk"
                (self.repo / legacy_rel).parent.mkdir(
                    parents=True, exist_ok=True
                )
                shutil.copyfile(self.repo / primary["path"], self.repo / legacy_rel)
                legacy = {"path": legacy_rel}
            self.current[block_id] = {
                "block_id": block_id,
                "length": primary["length"],
                "ordinal": index + 1,
                "sha256": primary["sha256"],
            }
            self.full_blocks.append(
                {
                    "block_id": block_id,
                    "length": primary["length"],
                    "ordinal": index + 1,
                    "sha256": primary["sha256"],
                    "tail_sha256": primary["tail_sha256"],
                    "replicas": [
                        {
                            "path": legacy["path"],
                            "partition": "p-legacy",
                            "priority": 1,
                        },
                        {
                            "path": primary["path"],
                            "partition": "p-primary",
                            "priority": 2,
                        },
                    ],
                }
            )

        full_digest = stable_state_digest(self.current.values())
        baseline = full_digest
        for sequence, block_id in ((1, "blk-0001"), (2, "blk-0002")):
            backup_id = f"inc-{sequence:03d}"
            generated = self._write(
                f"p-inc-{sequence:03d}",
                backup_id,
                block_id,
                f"inc-{sequence}",
                self.sizes[self.block_ids.index(block_id)],
            )
            self.current[block_id] = {
                "block_id": block_id,
                "length": generated["length"],
                "ordinal": self.block_ids.index(block_id) + 1,
                "sha256": generated["sha256"],
            }
            result_digest = stable_state_digest(self.current.values())
            self.increments.append(
                {
                    "sequence": sequence,
                    "backup_id": backup_id,
                    "baseline_digest": baseline,
                    "state_digest": result_digest,
                    "blocks": [
                        {
                            "block_id": block_id,
                            "length": generated["length"],
                            "ordinal": self.block_ids.index(block_id) + 1,
                            "sha256": generated["sha256"],
                            "tail_sha256": generated["tail_sha256"],
                            "replicas": [
                                {
                                    "path": generated["path"],
                                    "partition": f"p-inc-{sequence:03d}",
                                    "priority": 1,
                                }
                            ],
                        }
                    ],
                }
            )
            baseline = result_digest

        manifest = {
            "chain_id": "mini-chain",
            "schema_version": "backup-chain/v1",
            "block_file_suffix": ".chunk",
            "injection_log": "injections.json",
            "full": {
                "backup_id": "full-000",
                "expected_state_digest": full_digest,
                "blocks": self.full_blocks,
            },
            "increments": self.increments,
        }
        atomic_write_json(self.manifest_path, manifest)
        atomic_write_json(
            self.repo / "injections.json",
            {
                "events": [
                    {
                        "id": 1,
                        "type": "baseline_partition_copy",
                        "affected": [
                            {"backup_id": "full-000", "block_id": "blk-0002"}
                        ],
                    }
                ]
            },
        )

    def snapshot(self) -> dict[str, tuple[int, int, str]]:
        result = {}
        for path in sorted(self.repo.rglob("*")):
            if not path.is_file():
                continue
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            stat = path.stat()
            result[path.relative_to(self.repo).as_posix()] = (
                stat.st_size,
                stat.st_mtime_ns,
                digest,
            )
        return result


class ServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.repo = MiniRepository(self.root)
        self.db = self.root / "state.sqlite3"
        self.out = self.root / "out"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def service(self) -> BackupChainService:
        return BackupChainService(
            repo=self.repo.repo,
            manifest=self.repo.manifest_path,
            database=self.db,
            output_dir=self.out,
        )

    def test_audit_selects_digest_matching_copy(self) -> None:
        service = self.service()
        try:
            before = self.repo.snapshot()
            audit = service.audit()
            self.assertEqual(audit["catalog"]["usable_restore_points"], [0, 1, 2])
            self.assertEqual(audit["catalog"]["conflict_blocks"], 1)
            self.assertIsNone(audit["catalog"]["breakpoint"])
            blocks = service.store.restore_blocks(audit["audit_id"], 0)
            selected = {block["block_id"]: block for block in blocks}
            self.assertEqual(
                selected["blk-0002"]["path"],
                "p-primary/full-000/blk-0002.chunk",
            )
            self.assertEqual(selected["blk-0002"]["priority"], 2)
            status = service.status(
                chain_id=None, restore_point=0, audit_id=audit["audit_id"]
            )
            self.assertEqual(status["breakpoint"], None)
            self.assertEqual(
                status["recent_injection_impact"]["event"]["type"],
                "baseline_partition_copy",
            )
            drill = service.run_drill(
                restore_point=2, audit_id=audit["audit_id"]
            )
            self.assertEqual(drill["status"], "completed")
            conclusion_blocks = drill["conclusion"]["blocks"]
            self.assertEqual([item["block_id"] for item in conclusion_blocks], self.repo.block_ids)
            output = Path(drill["output_path"])
            offset = 0
            with output.open("rb") as handle:
                for item in conclusion_blocks:
                    handle.seek(offset)
                    data = handle.read(item["length"])
                    self.assertEqual(hashlib.sha256(data).hexdigest(), item["sha256"])
                    offset += item["length"]
            after = self.repo.snapshot()
            self.assertEqual(before, after)
            replay = service.run_drill(
                restore_point=2, audit_id=audit["audit_id"]
            )
            self.assertTrue(replay["idempotent_replay"])
            self.assertEqual(replay["drill_id"], drill["drill_id"])
        finally:
            service.close()

    def test_resume_reads_only_uncommitted_blocks(self) -> None:
        service = self.service()
        try:
            audit = service.audit()
            with patch.dict(
                os.environ, {"BACKUP_CHAIN_TEST_INTERRUPT_AFTER_BLOCKS": "1"}
            ):
                first = service.run_drill(
                    restore_point=2, audit_id=audit["audit_id"]
                )
            self.assertEqual(first["status"], "interrupted")
            self.assertEqual(first["progress"]["blocks"], 1)
            self.assertIsNone(first["output_path"])
            second = service.run_drill(
                restore_point=2, audit_id=audit["audit_id"]
            )
            self.assertEqual(second["status"], "completed")
            self.assertEqual(second["resume_count"], 1)
            self.assertEqual(second["blocks_skipped"], 1)
            self.assertEqual(second["blocks_read"], 3)
            self.assertEqual(second["blocks_written"], 3)
            self.assertEqual(len(second["conclusion"]["blocks"]), 3)
            events = second["conclusion"]["resume_events"]
            self.assertTrue(any(item["kind"] == "resume" for item in events))
            resume_event = next(item for item in events if item["kind"] == "resume")
            self.assertEqual(resume_event["from_progress_blocks"], 1)
        finally:
            service.close()

    def test_undeclared_copy_does_not_rescue_declared_missing_block(self) -> None:
        duplicate_rel = "p-copy/inc-001"
        source = self.repo.repo / "p-inc-001/inc-001"
        destination = self.repo.repo / duplicate_rel
        shutil.copytree(source, destination)
        service = self.service()
        try:
            first = service.audit()
            self.assertEqual(first["catalog"]["usable_restore_points"], [0, 1, 2])
            self.assertGreaterEqual(first["catalog"]["undeclared_copies"], 1)
            declared = (
                self.repo.repo / "p-inc-001/inc-001/blk-0001.chunk"
            )
            declared.unlink()
            second = service.audit()
            self.assertEqual(second["catalog"]["usable_restore_points"], [0])
            self.assertEqual(second["catalog"]["missing_blocks"], 1)
            self.assertEqual(
                second["catalog"]["breakpoint"]["code"], "BC_BLOCK_MISSING"
            )
            self.assertEqual(
                second["catalog"]["breakpoint"]["block_id"], "blk-0001"
            )
            audited = service.store.audit_blocks(second["audit_id"])
            target = next(
                block
                for block in audited
                if block["backup_id"] == "inc-001"
                and block["block_id"] == "blk-0001"
            )
            self.assertTrue(target["missing"])
            self.assertEqual(target["status"], "missing")
        finally:
            service.close()

    def test_replacement_breaks_chain_with_specific_code(self) -> None:
        path = self.repo.repo / "p-inc-001/inc-001/blk-0001.chunk"
        size = path.stat().st_size
        with path.open("r+b") as handle:
            handle.seek(min(100, size - 1))
            handle.write(b"REPLACED-CONTENT")
            handle.flush()
            os.fsync(handle.fileno())
        service = self.service()
        try:
            audit = service.audit()
            self.assertEqual(audit["catalog"]["usable_restore_points"], [0])
            self.assertEqual(
                audit["catalog"]["breakpoint"]["code"],
                "BC_BLOCK_DIGEST_MISMATCH",
            )
            self.assertEqual(audit["catalog"]["breakpoint"]["sequence"], 1)
            unavailable = service.status(
                chain_id=None,
                restore_point=1,
                audit_id=audit["audit_id"],
            )
            self.assertFalse(unavailable["restore_point"]["usable"])
            self.assertEqual(unavailable["selected_blocks"], [])
            self.assertEqual(
                unavailable["breakpoint"]["code"], "BC_BLOCK_DIGEST_MISMATCH"
            )
        finally:
            service.close()

    def test_drill_failure_records_expected_and_actual_without_output(self) -> None:
        service = self.service()
        try:
            audit = service.audit()
            path = self.repo.repo / "p-inc-001/inc-001/blk-0001.chunk"
            size = path.stat().st_size
            with path.open("r+b") as handle:
                handle.seek(size // 3)
                handle.write(b"POST-AUDIT-CHANGE")
                handle.flush()
                os.fsync(handle.fileno())
            drill = service.run_drill(
                restore_point=1, audit_id=audit["audit_id"]
            )
            self.assertEqual(drill["status"], "failed")
            self.assertIsNone(drill["output_path"])
            failure = drill["failure"]
            self.assertEqual(failure["block_id"], "blk-0001")
            self.assertEqual(failure["expected_sha256"], failure["detail"]["expected_sha256"])
            self.assertNotEqual(failure["expected_sha256"], failure["actual_sha256"])
            self.assertEqual(
                failure["code"], "BC_DRILL_BLOCK_DIGEST_MISMATCH"
            )
            failures = service.store.drill_failures(drill["drill_id"])
            self.assertEqual(len(failures), 1)
            self.assertEqual(
                failures[0]["expected_sha256"], failure["expected_sha256"]
            )
        finally:
            service.close()

    def test_state_path_inside_repository_is_rejected(self) -> None:
        with self.assertRaises(BusinessError) as raised:
            BackupChainService(
                repo=self.repo.repo,
                manifest=self.repo.manifest_path,
                database=self.repo.repo / "state.sqlite3",
                output_dir=self.out,
            )
        self.assertEqual(raised.exception.code, BC_DATABASE_PATH_INSIDE_REPO)

    def test_old_audit_selection_is_immutable(self) -> None:
        service = self.service()
        try:
            first = service.audit()
            manifest = json.loads(self.repo.manifest_path.read_text())
            for block in manifest["full"]["blocks"]:
                priorities = [replica["priority"] for replica in block["replicas"]]
                for replica, priority in zip(block["replicas"], priorities):
                    replica["priority"] = priorities[-1] + priorities[0] - priority
            atomic_write_json(self.repo.manifest_path, manifest)
            second = service.audit()
            self.assertNotEqual(first["audit_id"], second["audit_id"])
            with sqlite3.connect(self.db) as connection:
                rows = connection.execute(
                    """
                    SELECT audit_id, path, priority FROM restore_point_blocks
                    WHERE sequence=0 AND block_id='blk-0002'
                    ORDER BY audit_id
                    """
                ).fetchall()
            self.assertEqual(rows[0][2], 2)
            self.assertEqual(rows[1][2], 1)
            self.assertEqual(rows[0][1], rows[1][1])
        finally:
            service.close()


if __name__ == "__main__":
    unittest.main()
