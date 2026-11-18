from __future__ import annotations

import hashlib
import json
import tempfile
import threading
import time
import unittest
from collections import Counter
from pathlib import Path

from wal_archive.errors import ArchiveError
from wal_archive.archive import scan_archive
from wal_archive.samplegen import build_segment_bytes, generate_archive
from wal_archive.service import ArchiveService
from wal_archive.wal import DEFAULT_SEGMENT_SIZE, wal_filename


class ServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name) / "archive"
        self.generated = generate_archive(self.root, profile="small")
        self.db = Path(self.tmp.name) / "catalog.sqlite3"
        self.service = ArchiveService(self.root, self.db)
        self.service.scan()

    def tearDown(self) -> None:
        self.service.close()
        self.tmp.cleanup()

    def _version(self, instance: str, timeline: int, segno: int) -> list:
        return [
            row
            for row in self.service.store.current_versions(
                instance=instance, timeline=timeline
            )
            if int(row["segno"]) == segno
        ]

    def test_scan_classifies_and_reports_bad_entries(self):
        rows = self.service.store.current_versions()
        classifications = Counter(row["classification"] for row in rows)
        self.assertEqual(classifications["complete"], 36)
        self.assertEqual(classifications["truncated"], 2)
        self.assertEqual(classifications["checksum_failed"], 2)
        self.assertEqual(len(self.service.store.current_in_progress()), 3)
        errors = self.service.store.current_errors()
        self.assertEqual([item.code for item in errors], ["WAL002", "WAL003"])
        self.assertEqual(
            errors[1].rel_path,
            "instances/pg-55432/segments/not-a-wal-segment.wal",
        )
        self.assertTrue(all("not-a-wal-segment" not in row["filename"] for row in rows))
        self.assertEqual(sum(1 for row in rows if row["conflict"]), 4)

    def test_recovery_chain_gap_and_timeline_marker(self):
        result = self.service.resolve_recovery(
            instance="pg-55432",
            timeline=1,
            target_time="2026-01-01T00:00:14Z",
        )
        point = result["recovery_point"]
        self.assertEqual(point["head_segno"], 6)
        self.assertEqual(point["chain_length"], 6)
        self.assertEqual(
            [item["segno"] for item in point["chain"]], [1, 2, 3, 4, 5, 6]
        )
        self.assertEqual(point["validation"]["classification"], "complete")
        self.assertEqual(point["validation"]["pages_checked"], 384)
        seg3 = next(item for item in point["selection"] if item["segno"] == 3)
        self.assertEqual(seg3["selection_reason"], "timeline_marker_authoritative_instance")
        self.assertEqual(seg3["selected_instance"], "pg-55432")
        a_candidate = next(
            item for item in seg3["candidates"] if item["instance"] == "pg-55432"
        )
        self.assertEqual(seg3["selected_digest"], a_candidate["digest"])

        with self.assertRaises(ArchiveError) as caught:
            self.service.resolve_recovery(
                instance="pg-55432",
                timeline=1,
                target_time="2026-01-01T00:00:30Z",
            )
        self.assertEqual(caught.exception.code, "WAL011")
        self.assertEqual(caught.exception.details["gaps"], [7, 8])
        self.assertEqual(len(self.service.store.list_recoveries()), 1)

        t2 = self.service.resolve_recovery(
            instance="pg-55432",
            timeline=2,
            target_time="2026-01-01T00:02:10Z",
        )["recovery_point"]
        self.assertEqual(t2["head_segno"], 6)
        self.assertEqual(t2["chain_length"], 6)
        self.assertEqual(t2["chain"][0]["selected_version"]["instance"], "pg-55433")
        self.assertEqual(t2["chain"][-1]["selected_version"]["instance"], "pg-55432")

    def test_restart_reuses_committed_point(self):
        first = self.service.resolve_recovery(
            instance="pg-55432",
            timeline=1,
            target_time="2026-01-01T00:00:14Z",
        )["recovery_point"]
        self.service.close()
        self.service = ArchiveService(self.root, self.db)
        restored = self.service.status(instance="pg-55432", timeline=1)
        self.assertEqual(restored["chain_length"], 6)
        self.assertEqual(restored["current_recovery_point"]["id"], first["id"])
        self.service.scan()
        second = self.service.resolve_recovery(
            instance="pg-55432",
            timeline=1,
            target_time="2026-01-01T00:00:14Z",
            refresh=True,
        )
        self.assertTrue(second["reused"])
        self.assertEqual(second["recovery_point"]["id"], first["id"])
        self.assertEqual(second["recovery_point"]["chain_length"], 6)

    def test_truncated_path_never_becomes_complete(self):
        timeline = 1
        segno = 1
        a_path = (
            self.root
            / "instances"
            / "pg-55432"
            / "segments"
            / wal_filename(timeline, segno, DEFAULT_SEGMENT_SIZE)
        )
        b_path = (
            self.root
            / "instances"
            / "pg-55433"
            / "segments"
            / wal_filename(timeline, segno, DEFAULT_SEGMENT_SIZE)
        )
        replacement = b_path.read_bytes()
        with open(a_path, "r+b") as handle:
            handle.truncate(96 * 1024)
        self.service.scan()
        rows = self._version("pg-55432", timeline, segno)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["classification"], "truncated")

        self.service.close()
        self.service = ArchiveService(self.root, self.db)
        self.service.scan()
        a_path.write_bytes(replacement)
        self.service.scan()
        rows = [
            row
            for row in self.service.store.all_versions(
                instance="pg-55432", timeline=timeline, segno=segno
            )
        ]
        self.assertEqual(len(rows), 2)
        current = [row for row in rows if row["last_scan_id"] == self.service.store.latest_scan_id()]
        self.assertEqual(len(current), 1)
        self.assertEqual(current[0]["classification"], "truncated")
        self.assertIn("sticky", current[0]["reason"])

    def test_live_writer_is_excluded(self):
        segno = 30
        name = wal_filename(1, segno, DEFAULT_SEGMENT_SIZE)
        path = self.root / "instances" / "pg-55432" / "segments" / name
        path.write_bytes(b"\xA5" * (64 * 1024))
        stop = threading.Event()

        def writer() -> None:
            with open(path, "ab", buffering=0) as handle:
                while not stop.is_set():
                    handle.write(b"\xA5" * 1024)
                    time.sleep(0.03)

        thread = threading.Thread(target=writer, daemon=True)
        thread.start()
        try:
            time.sleep(0.05)
            self.service.scan()
            writing = {
                item.filename: item for item in self.service.store.current_in_progress()
            }
            self.assertIn(name, writing)
            self.assertIn(
                writing[name].reason,
                {
                    "size_or_mtime_changed_during_probe",
                    "changed_during_validation",
                },
            )
        finally:
            stop.set()
            thread.join(timeout=2)
        self.assertFalse(any(
            row["filename"] == name
            for row in self.service.store.current_versions()
        ))

    def test_validation_does_not_modify_archive(self):
        before = {}
        for path in sorted(self.root.rglob("*")):
            if path.is_file():
                stat = path.stat()
                before[path.relative_to(self.root).as_posix()] = (
                    stat.st_size,
                    stat.st_mtime_ns,
                    hashlib.sha256(path.read_bytes()).hexdigest(),
                )
        self.service.scan()
        self.service.resolve_recovery(
            instance="pg-55432",
            timeline=1,
            target_time="2026-01-01T00:00:14Z",
            refresh=False,
        )
        after = {}
        for path in sorted(self.root.rglob("*")):
            if path.is_file():
                stat = path.stat()
                after[path.relative_to(self.root).as_posix()] = (
                    stat.st_size,
                    stat.st_mtime_ns,
                    hashlib.sha256(path.read_bytes()).hexdigest(),
                )
        self.assertEqual(before, after)

    def test_report_is_repeatable_and_sorted(self):
        self.service.resolve_recovery(
            instance="pg-55432",
            timeline=1,
            target_time="2026-01-01T00:00:14Z",
        )
        first = self.service.report()
        self.service.scan()
        second = self.service.report()
        for payload in (first, second):
            payload.pop("generated_at", None)
            payload.pop("duration_ms", None)
        self.assertEqual(first, second)
        instances = [item["instance"] for item in first["instances"]]
        self.assertEqual(instances, sorted(instances))
        for item in first["instances"]:
            timelines = [entry["timeline"] for entry in item["timelines"]]
            self.assertEqual(timelines, sorted(timelines))
        recovery_keys = [
            (item["instance"], item["timeline"], item["target_time"], item["id"])
            for item in first["recovery_points"]
        ]
        self.assertEqual(recovery_keys, sorted(recovery_keys))
        chain = first["recovery_points"][0]["chain"]
        self.assertTrue(chain)
        self.assertIn("instance", chain[0]["selected_version"])
        self.assertIn("digest", chain[0]["selected_version"])
        timeline = first["instances"][0]["timelines"][0]
        for field in (
            "segment_count",
            "usable_bytes",
            "truncated_segments",
            "conflict_versions",
            "gap_segment_numbers",
            "verification",
        ):
            self.assertIn(field, timeline)

    def test_metrics_and_status_contract(self):
        self.service.resolve_recovery(
            instance="pg-55432",
            timeline=1,
            target_time="2026-01-01T00:00:14Z",
        )
        metrics = self.service.metrics()
        for metric in (
            "wal_archive_segments_total",
            "wal_archive_usable_bytes",
            "wal_archive_writing_segments",
            "wal_archive_truncated_segments_total",
            "wal_archive_checksum_failures_total",
            "wal_archive_conflict_versions_total",
            "wal_archive_committed_recovery_points_total",
        ):
            self.assertIn(metric, metrics)
        self.assertIn(
            'wal_archive_segments_total{instance="pg-55432",timeline="1"} 18',
            metrics,
        )
        status = self.service.status(instance="pg-55432", timeline=1)
        self.assertEqual(status["chain_length"], 6)
        self.assertEqual(status["counts"]["gap_segment_numbers"], [7, 8])
        self.assertEqual(status["counts"]["conflict_versions"], 3)
        encoded = json.dumps(status, sort_keys=True)
        self.assertNotIn(str(self.root), encoded)
        self.assertNotIn(str(self.root.parent), encoded)

    def test_historical_conflict_versions_remain_queryable(self):
        segno = 1
        name = wal_filename(1, segno, DEFAULT_SEGMENT_SIZE)
        path = self.root / "instances" / "pg-55432" / "segments" / name
        changed = build_segment_bytes(1, segno, variant=77)
        path.write_bytes(changed)
        self.service.scan()
        conflicts = self.service.find_conflicts(timeline=1, segno=segno)
        self.assertEqual(len(conflicts["conflicts"]), 1)
        versions = conflicts["conflicts"][0]["versions"]
        self.assertGreaterEqual(len(versions), 3)
        self.assertTrue(any(not item["current"] for item in versions))
        current_versions = [item for item in versions if item["current"]]
        self.assertTrue(current_versions)
        self.assertTrue(any(item["conflict"] for item in current_versions))
        self.assertGreaterEqual(len({item["digest"] for item in versions}), 2)

    def test_unknown_instance_and_bad_target(self):
        with self.assertRaises(ArchiveError) as caught:
            self.service.status(instance="does-not-exist")
        self.assertEqual(caught.exception.code, "WAL008")
        with self.assertRaises(ArchiveError) as caught:
            self.service.resolve_recovery(
                instance="pg-55432",
                timeline=1,
                target_time="not-a-time",
            )
        self.assertEqual(caught.exception.code, "WAL007")

    def test_missing_instances_directory_is_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            snapshot = scan_archive(tmp)
            self.assertEqual(len(snapshot.errors), 1)
            self.assertEqual(snapshot.errors[0].code, "WAL002")
            self.assertEqual(snapshot.errors[0].rel_path, "instances")


if __name__ == "__main__":
    unittest.main()
