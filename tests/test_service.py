from __future__ import annotations

import concurrent.futures
import tempfile
import threading
import unittest
import urllib.parse
from pathlib import Path

import requests

from archive_bucket.http_server import BucketRequestHandler, create_server
from archive_bucket.service import ArchiveService

BucketRequestHandler.log_message = lambda self, fmt, *args: None


class ServiceIntegrationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory(prefix="bucket-tests-")
        self.root = Path(self.tempdir.name)
        self.server = None
        self.thread = None
        self.base_url = ""
        self._start()

    def tearDown(self) -> None:
        self._stop()
        self.tempdir.cleanup()

    def _start(self) -> None:
        self.service = ArchiveService(self.root)
        self.server = create_server(self.service, "127.0.0.1", 0)
        self.base_url = self.service.base_url
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def _stop(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
            if self.thread is not None:
                self.thread.join(timeout=5)
            self.server = None
            self.thread = None

    def _request(self, method: str, path: str, **kwargs: object) -> requests.Response:
        with requests.Session() as session:
            session.trust_env = False
            return session.request(
                method,
                self.base_url + path,
                timeout=30,
                **kwargs,
            )

    def _json(self, method: str, path: str, **kwargs: object) -> dict:
        response = self._request(method, path, **kwargs)
        self.assertLess(response.status_code, 400, response.text)
        return response.json()

    @staticmethod
    def _object_path(bucket: str, key: str) -> str:
        return f"/v1/buckets/{bucket}/objects/" + urllib.parse.quote(key, safe="")

    def _put(self, key: str, data: bytes, bucket: str = "main") -> dict:
        response = self._request("PUT", self._object_path(bucket, key), data=data)
        self.assertEqual(201, response.status_code, response.text)
        return response.json()

    def test_revisions_delete_and_rollback(self) -> None:
        key = "archive/sample.bin"
        first = self._put(key, b"first revision")
        second = self._put(key, b"second revision")
        self.assertGreater(second["revision_id"], first["revision_id"])
        self.assertGreater(second["commit_seq"], first["commit_seq"])

        latest = self._request("GET", self._object_path("main", key))
        self.assertEqual(b"second revision", latest.content)
        self.assertEqual(str(second["revision_id"]), latest.headers["X-Object-Revision-Id"])
        old = self._request(
            "GET",
            self._object_path("main", key) + f"/revisions/{first['revision_id']}",
        )
        self.assertEqual(b"first revision", old.content)

        snapshot = self._json("POST", "/v1/buckets/main/snapshots", json={})
        self.assertEqual({key: second["revision_id"]}, snapshot["visible"])
        deleted = self._json("DELETE", self._object_path("main", key))
        self.assertGreater(deleted["commit_seq"], second["commit_seq"])
        self.assertEqual(404, self._request("GET", self._object_path("main", key)).status_code)
        history = self._request(
            "GET",
            self._object_path("main", key) + f"/revisions/{second['revision_id']}",
        )
        self.assertEqual(200, history.status_code)

        restored = self._json(
            "POST",
            f"/v1/buckets/main/snapshots/{snapshot['snapshot_id']}/restore",
        )
        self.assertEqual(second["revision_id"], self._json("GET", "/v1/buckets/main/view")["visible"][key])
        self.assertGreater(restored["commit_seq"], deleted["commit_seq"])
        retained = self._request(
            "GET",
            self._object_path("main", key) + f"/revisions/{first['revision_id']}",
        )
        self.assertEqual(b"first revision", retained.content)

    def test_snapshot_does_not_mix_concurrent_writes(self) -> None:
        keys = [f"archive/{index:03d}.bin" for index in range(20)]
        old = {}
        for key in keys:
            old[key] = self._put(key, f"old:{key}".encode())["revision_id"]
        initial = self._json("POST", "/v1/buckets/main/snapshots", json={})
        self.assertEqual(old, initial["visible"])

        def update(chunk: list[str]) -> dict[str, int]:
            result = {}
            for key in chunk:
                result[key] = self._put(key, f"new:{key}".encode())["revision_id"]
            return result

        midpoint = len(keys) // 2
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures = [
                pool.submit(update, keys[:midpoint]),
                pool.submit(update, keys[midpoint:]),
            ]
            snapshot = self._json("POST", "/v1/buckets/main/snapshots", json={})
            updated = {}
            for future in futures:
                updated.update(future.result())
        view = self._json(
            "GET",
            f"/v1/buckets/main/view?commit_seq={snapshot['commit_seq']}",
        )
        self.assertEqual(view["visible"], snapshot["visible"])
        for key, revision in snapshot["visible"].items():
            self.assertIn(revision, {old[key], updated[key]})

    def test_incremental_sync_pause_failure_resume_and_restart(self) -> None:
        for index in range(6):
            self._put(f"archive/sync-{index}.bin", f"payload-{index}".encode())
        deleted_key = "archive/sync-5.bin"
        self._json("DELETE", self._object_path("main", deleted_key))

        self._json(
            "POST",
            "/v1/faults",
            json={"sync_pause": False, "sync_pause_after": 2},
        )
        paused = self._json("POST", "/v1/sync", json={"source": self.base_url})
        self.assertTrue(paused["paused"])
        self.assertEqual(2, paused["applied_since_start"])
        self.assertGreater(paused["lag_commits"], 0)

        self._json(
            "POST",
            "/v1/faults",
            json={
                "sync_pause": False,
                "sync_pause_after": 0,
                "backup_write_failure_once": True,
            },
        )
        before = self._json("GET", "/v1/buckets/backup/stats")["applied_seq"]
        failed = self._request("POST", "/v1/sync", json={"source": self.base_url})
        self.assertEqual(503, failed.status_code)
        self.assertEqual(
            before,
            self._json("GET", "/v1/buckets/backup/stats")["applied_seq"],
        )

        self._json("POST", "/v1/faults", json={"sync_pause": False})
        resumed = self._json("POST", "/v1/sync", json={"source": self.base_url})
        self.assertEqual(0, resumed["lag_commits"])
        self.assertGreater(resumed["applied_seq"], paused["applied_seq"])
        comparison = self._json("POST", "/v1/consistency/check", json={})
        self.assertTrue(comparison["consistent"], comparison)

        applied_before_restart = resumed["applied_seq"]
        main_view = self._json("GET", "/v1/buckets/main/view")["visible"]
        self._stop()
        self._start()
        self.assertEqual(
            applied_before_restart,
            self._json("GET", "/v1/buckets/backup/stats")["applied_seq"],
        )
        self.assertEqual(
            main_view,
            self._json("GET", "/v1/buckets/backup/view")["visible"],
        )
        self.assertTrue(self._json("POST", "/v1/consistency/check", json={})["consistent"])

    def test_gc_only_reclaims_unreferenced_invisible_revisions(self) -> None:
        key = "archive/gc.bin"
        first = self._put(key, b"v1")
        second = self._put(key, b"v2")
        snapshot = self._json("POST", "/v1/buckets/main/snapshots", json={})
        self.assertEqual(second["revision_id"], snapshot["visible"][key])
        self._json("DELETE", self._object_path("main", key))
        self._json("POST", "/v1/sync", json={"source": self.base_url})
        gc = self._json("POST", "/v1/buckets/main/gc", json={})
        self.assertGreaterEqual(gc["reclaimed_revision_count"], 1)
        self.assertGreaterEqual(gc["reclaimed_bytes"], 2)

        first_read = self._request(
            "GET",
            self._object_path("main", key) + f"/revisions/{first['revision_id']}",
        )
        self.assertEqual(410, first_read.status_code)
        second_read = self._request(
            "GET",
            self._object_path("main", key) + f"/revisions/{second['revision_id']}",
        )
        self.assertEqual(b"v2", second_read.content)
        stats = self._json("GET", "/v1/buckets/main/stats")
        self.assertEqual(1, stats["snapshot_referenced_revisions"])
        self.assertGreaterEqual(stats["reclaimed_revision_count"], 1)

    def test_consistency_classifies_missing_behind_and_content_difference(self) -> None:
        keys = ["archive/a.bin", "archive/b.bin", "archive/c.bin", "archive/d.bin"]
        for key in keys:
            self._put(key, f"content:{key}".encode())
        synced = self._json("POST", "/v1/sync", json={"source": self.base_url})
        self.assertEqual(0, synced["lag_commits"])

        # Content corruption: same revision id and metadata, different bytes.
        a_revision = self._json("GET", self._object_path("main", keys[0]) + "/revisions")
        content_hash = a_revision["revisions"][0]["content_hash"]
        self.service.backup._blob_path(content_hash).write_bytes(b"corrupt!")

        # Backup-only newer revision.
        self._put(keys[1], b"replica-local-revision", bucket="backup")

        # Backup-only tombstone.
        self._json("DELETE", self._object_path("backup", keys[2]))

        # Missing blob content for the same visible revision.
        d_revision = self._json("GET", self._object_path("main", keys[3]) + "/revisions")
        d_hash = d_revision["revisions"][0]["content_hash"]
        self.service.backup._blob_path(d_hash).unlink()

        result = self._json("POST", "/v1/consistency/check", json={})
        self.assertFalse(result["consistent"])
        self.assertEqual(1, result["reason_counts"]["content_different"], result)
        self.assertEqual(1, result["reason_counts"]["revision_behind"], result)
        self.assertEqual(2, result["reason_counts"]["missing"], result)
        by_key = {item["key"]: item for item in result["inconsistencies"]}
        self.assertEqual("content_different", by_key[keys[0]]["reason"])
        self.assertEqual("revision_behind", by_key[keys[1]]["reason"])
        self.assertEqual("missing", by_key[keys[2]]["reason"])
        self.assertEqual("missing", by_key[keys[3]]["reason"])

    def test_status_metrics_shape_and_no_absolute_paths(self) -> None:
        self._put("archive/status.bin", b"status")
        status_response = self._request("GET", "/v1/status")
        status = status_response.json()
        self.assertEqual(sorted(status), list(status.keys()))
        self.assertNotIn(str(self.root), status_response.text)
        self.assertIn("sync", status)
        self.assertEqual({"main", "backup"}, set(status["buckets"]))
        self.assertEqual(1, status["buckets"]["main"]["visible_object_count"])

        metrics = self._request("GET", "/metrics")
        self.assertEqual(200, metrics.status_code)
        self.assertIn("object_bucket_visible_objects{bucket=\"main\"} 1", metrics.text)
        for name in (
            "object_bucket_occupancy_bytes",
            "object_bucket_latest_commit_seq",
            "object_bucket_synced_commit_seq",
            "object_bucket_snapshots",
            "object_bucket_reclaimed_bytes",
            "object_bucket_consistency_mismatch_keys",
        ):
            self.assertIn(name, metrics.text)


if __name__ == "__main__":
    unittest.main()
