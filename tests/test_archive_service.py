from __future__ import annotations

import http.client
import json
import tempfile
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from datetime import timedelta
from pathlib import Path

from archive_service.errors import ApiError
from archive_service.http_server import make_server
from archive_service.service import ArchiveService, DOMAINS
from archive_service.util import format_time, parse_time, utc_now


class ArchiveServiceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="archive-test-")
        self.service = ArchiveService(
            self.temp.name,
            bootstrap_regular_days=180,
            bootstrap_audit_days=180,
            bootstrap_legal_days=180,
        )
        self.server = None

    def tearDown(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        self.temp.cleanup()

    def _write(
        self,
        key: str,
        content: bytes = b"report-data",
        *,
        category: str = "audit",
        legal_hold: bool = False,
        written_at: str | None = None,
        fail_domains: list[str] | None = None,
    ) -> dict:
        source = self.service.new_temp_path()
        source.write_bytes(content)
        import hashlib

        result, _ = self.service.write_object(
            object_key=key,
            source_path=source,
            content_length=len(content),
            content_sha256=hashlib.sha256(content).hexdigest(),
            category=category,
            legal_hold=legal_hold,
            written_at=written_at,
            fail_domains=fail_domains,
        )
        source.unlink(missing_ok=True)
        return result

    @staticmethod
    def _paths(service: ArchiveService, version_id: str) -> list[Path]:
        return [service._domain_path(domain, version_id) for domain in DOMAINS]

    def test_policy_extension_and_shortening_rejection(self) -> None:
        old_written = utc_now() - timedelta(days=210)
        version = self._write("audit/report-1", written_at=format_time(old_written))
        original_deadline = format_time(old_written + timedelta(days=180))
        self.assertEqual(version["retention_until"], original_deadline)

        publication = self.service.publish_policy(
            regular_days=365,
            audit_days=365,
            legal_days=365,
        )
        self.assertGreaterEqual(publication["changed_version_count"], 1)
        metadata = self.service.version_metadata(version["version_id"])
        expected_deadline = format_time(old_written + timedelta(days=365))
        self.assertEqual(metadata["retention_until"], expected_deadline)
        self.assertEqual(metadata["lock"]["retention_until"], expected_deadline)
        publication_report = self.service.policy_impact_for_publication(
            publication["publication_id"],
            as_of=format_time(utc_now()),
        )
        self.assertEqual(
            publication_report["retention_deadline_changed_count"], 1
        )
        self.assertEqual(
            publication_report["cleanup_admission_changes"][0]["version_id"],
            version["version_id"],
        )

        active_before = self.service.current_policy()["active"]
        with self.assertRaises(ApiError) as raised:
            self.service.publish_policy(
                regular_days=180,
                audit_days=180,
                legal_days=180,
            )
        self.assertEqual(
            raised.exception.code, "RETENTION_POLICY_SHORTENING_REJECTED"
        )
        self.assertEqual(raised.exception.status, 409)
        active_after = self.service.current_policy()["active"]
        self.assertEqual(
            active_before["publication_id"], active_after["publication_id"]
        )
        self.assertEqual(active_after["retention_days"]["audit"], 365)

    def test_future_policy_uses_effective_at_for_new_versions(self) -> None:
        now = utc_now()
        effective = now + timedelta(days=1)
        self.service.publish_policy(
            regular_days=365,
            audit_days=365,
            legal_days=365,
            effective_at=format_time(effective),
        )
        before = self._write(
            "audit/before-effective",
            b"before",
            written_at=format_time(now),
        )
        after = self._write(
            "audit/after-effective",
            b"after",
            written_at=format_time(effective + timedelta(hours=1)),
        )
        self.assertEqual(
            before["retention_until"],
            format_time(now + timedelta(days=180)),
        )
        self.assertEqual(
            after["retention_until"],
            format_time(effective + timedelta(hours=1, days=365)),
        )

    def test_concurrent_policy_publish_and_writes_keep_lock_in_sync(self) -> None:
        def write(index: int) -> dict:
            return self._write(
                f"audit/concurrent-{index}",
                f"content-{index}".encode("ascii"),
                category="audit",
            )

        with ThreadPoolExecutor(max_workers=5) as executor:
            futures = [executor.submit(write, index) for index in range(12)]
            publication = self.service.publish_policy(
                regular_days=365,
                audit_days=365,
                legal_days=365,
            )
            versions = [future.result(timeout=10) for future in futures]
        self.assertEqual(publication["retention_days"]["audit"], 365)
        for version in versions:
            metadata = self.service.version_metadata(version["version_id"])
            self.assertEqual(
                metadata["retention_until"],
                metadata["lock"]["retention_until"],
            )
            self.assertGreaterEqual(
                metadata["retention_until"],
                format_time(
                    parse_time(metadata["written_at"]) + timedelta(days=180)
                ),
            )

    def test_conflicting_lock_is_denied_without_domain_deletion(self) -> None:
        old_written = utc_now() - timedelta(days=210)
        version = self._write("audit/report-conflict", written_at=format_time(old_written))
        self.service.publish_policy(
            regular_days=365,
            audit_days=365,
            legal_days=365,
        )
        metadata_deadline = format_time(old_written + timedelta(days=365))
        lock_deadline = format_time(old_written + timedelta(days=180))
        self.service.override_retention(
            version["version_id"],
            metadata_retention_until=metadata_deadline,
            lock_retention_until=lock_deadline,
            lock_pinned=True,
        )

        admission = self.service.admission(version["version_id"])
        self.assertFalse(admission["allowed"])
        self.assertEqual(admission["reason_code"], "RETENTION_CONFLICT_ACTIVE")
        self.assertIn(
            "RETENTION_DEADLINE_MISMATCH", admission["reason_codes"]
        )
        self.assertEqual(
            admission["retention_resolution"]["effective_retention_until"],
            metadata_deadline,
        )
        self.assertEqual(
            admission["conflict"]["fields"],
            [
                "object_versions.retention_until",
                "lock_records.retention_until",
            ],
        )
        paths = self._paths(self.service, version["version_id"])
        with self.assertRaises(ApiError) as raised:
            self.service.delete_version(version["version_id"])
        self.assertEqual(raised.exception.code, "RETENTION_CONFLICT_ACTIVE")
        self.assertTrue(all(path.is_file() for path in paths))
        self.assertIn(
            "archive_retention_conflict_delete_denials_total 1",
            self.service.metrics_text(),
        )

    def test_legal_hold_denies_even_when_retention_expired(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archive-hold-") as directory:
            service = ArchiveService(
                directory,
                bootstrap_regular_days=0,
                bootstrap_audit_days=0,
                bootstrap_legal_days=0,
            )
            version = self._write_into(
                service,
                "legal/sealed",
                b"sealed",
                category="audit",
                legal_hold=True,
            )
            self.assertIsNone(version["earliest_deletable_at"])
            admission = service.admission(version["version_id"])
            self.assertFalse(admission["allowed"])
            self.assertEqual(admission["reason_code"], "LEGAL_HOLD_ACTIVE")
            with self.assertRaises(ApiError):
                service.delete_version(version["version_id"])
            self.assertTrue(
                all(
                    service._domain_path(domain, version["version_id"]).is_file()
                    for domain in DOMAINS
                )
            )

    def _write_into(
        self,
        service: ArchiveService,
        key: str,
        content: bytes,
        *,
        category: str = "regular",
        legal_hold: bool = False,
        fail_domains: list[str] | None = None,
    ) -> dict:
        import hashlib

        source = service.new_temp_path()
        source.write_bytes(content)
        result, _ = service.write_object(
            object_key=key,
            source_path=source,
            content_length=len(content),
            content_sha256=hashlib.sha256(content).hexdigest(),
            category=category,
            legal_hold=legal_hold,
            fail_domains=fail_domains,
        )
        source.unlink(missing_ok=True)
        return result

    def test_partial_write_reports_missing_domain(self) -> None:
        version = self._write(
            "regular/partial",
            b"partial",
            category="regular",
            fail_domains=["replica-b"],
        )
        self.assertFalse(version["complete"])
        self.assertEqual(version["status"], "partial")
        self.assertEqual(version["missing_domains"], ["replica-b"])
        domain_status = {
            item["domain"]: item["write_status"]
            for item in self.service.version_metadata(version["version_id"])["domains"]
        }
        self.assertEqual(domain_status["replica-b"], "missing")

    def test_cleanup_resume_keeps_lock_until_all_domains_finish(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archive-clean-") as directory:
            service = ArchiveService(
                directory,
                bootstrap_regular_days=0,
                bootstrap_audit_days=0,
                bootstrap_legal_days=0,
            )
            first = self._write_into(service, "regular/v1", b"version-one")
            second = self._write_into(service, "regular/v1", b"version-two")
            future = format_time(utc_now() + timedelta(days=30))
            service.override_retention(
                second["version_id"],
                metadata_retention_until=future,
                lock_retention_until=future,
            )
            first_cleanup = service.cleanup(fail_domains=["replica-a"])
            self.assertEqual(first_cleanup["failed_version_count"], 1)
            self.assertEqual(first_cleanup["failed_domains"], ["replica-a"])
            first_meta = service.version_metadata(first["version_id"])
            self.assertEqual(first_meta["status"], "cleanup_partial")
            self.assertIsNotNone(first_meta["lock"])
            domain_status = {
                item["domain"]: item["delete_status"]
                for item in first_meta["domains"]
            }
            self.assertEqual(domain_status["primary"], "deleted")
            self.assertEqual(domain_status["replica-a"], "failed")
            self.assertEqual(domain_status["replica-b"], "present")
            self.assertTrue(
                service._domain_path("replica-b", first["version_id"]).exists()
            )
            _, read_meta = service.read_version(second["version_id"])
            self.assertEqual(read_meta["version_id"], second["version_id"])

            second_cleanup = service.cleanup()
            self.assertEqual(second_cleanup["failed_version_count"], 0)
            final_meta = service.version_metadata(first["version_id"])
            self.assertEqual(final_meta["status"], "deleted")
            self.assertIsNone(final_meta["lock"])
            self.assertFalse(
                any(
                    service._domain_path(domain, first["version_id"]).exists()
                    for domain in DOMAINS
                )
            )
            self.assertTrue(
                all(
                    service._domain_path(domain, second["version_id"]).exists()
                    for domain in DOMAINS
                )
            )

    def test_reconciliation_reports_three_difference_classes(self) -> None:
        version = self._write("audit/reconcile", b"abcdefghij" * 100)
        self.service.create_domain_difference(
            version["version_id"], domain="replica-a", mode="missing"
        )
        self.service.create_domain_difference(
            version["version_id"], domain="replica-b", mode="truncate"
        )
        result = self.service.reconcile()
        self.assertEqual(result["snapshot_version_count"], 1)
        self.assertGreaterEqual(result["difference_count"], 3)
        self.assertEqual(
            {item["domain"] for item in result["missing_domains"]},
            {"replica-a"},
        )
        self.assertEqual(
            {item["domain"] for item in result["length_mismatches"]},
            {"replica-b"},
        )
        self.assertEqual(
            {item["domain"] for item in result["digest_mismatches"]},
            {"replica-b"},
        )
        self.assertTrue(
            all(
                item["version_id"] == version["version_id"]
                for item in result["differences"]
            )
        )
        self.assertIn(
            f'archive_reconciliation_differences {result["difference_count"]}',
            self.service.metrics_text(),
        )

    def test_reconciliation_excludes_deleted_snapshot_versions(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archive-reconcile-") as directory:
            service = ArchiveService(
                directory,
                bootstrap_regular_days=0,
                bootstrap_audit_days=0,
                bootstrap_legal_days=0,
            )
            deleted = self._write_into(service, "regular/deleted", b"gone")
            active = self._write_into(service, "regular/active", b"still-here")
            future = format_time(utc_now() + timedelta(days=30))
            service.override_retention(
                active["version_id"],
                metadata_retention_until=future,
                lock_retention_until=future,
            )
            service.cleanup()
            service.create_domain_difference(
                active["version_id"], domain="primary", mode="missing"
            )
            result = service.reconcile()
            self.assertEqual(result["snapshot_version_count"], 1)
            self.assertFalse(
                any(
                    item["version_id"] == deleted["version_id"]
                    for item in result["differences"]
                )
            )
            self.assertTrue(
                all(
                    item["version_id"] == active["version_id"]
                    for item in result["differences"]
                )
            )

    def test_impact_report_is_deterministic_and_lists_admission_changes(self) -> None:
        old_written = utc_now() - timedelta(days=210)
        version = self._write("audit/impact", written_at=format_time(old_written))
        as_of = format_time(utc_now())
        effective_at = format_time(utc_now() + timedelta(days=1))
        first = self.service.policy_impact(
            regular_days=365,
            audit_days=365,
            legal_days=365,
            as_of=as_of,
            effective_at=effective_at,
        )
        second = self.service.policy_impact(
            regular_days=365,
            audit_days=365,
            legal_days=365,
            as_of=as_of,
            effective_at=effective_at,
        )
        first.pop("generated_at")
        second.pop("generated_at")
        self.assertEqual(first, second)
        self.assertEqual(first["retention_deadline_changed_count"], 1)
        self.assertTrue(first["cleanup_admission_changes"])
        self.assertEqual(
            first["cleanup_admission_changes"][0]["version_id"],
            version["version_id"],
        )

    def test_status_and_metrics_do_not_expose_absolute_paths(self) -> None:
        self._write("audit/status", b"status")
        status_text = json.dumps(self.service.status(), sort_keys=True)
        self.assertNotIn(str(Path(self.temp.name).resolve()), status_text)
        metrics = self.service.metrics_text()
        for name in (
            "archive_domain_object_versions",
            "archive_domain_bytes",
            "archive_pending_cleanup_versions",
            "archive_retention_conflict_delete_denials_total",
            "archive_reconciliation_differences",
        ):
            self.assertIn(name, metrics)
        self.assertNotIn(str(Path(self.temp.name).resolve()), metrics)

    def test_http_write_read_and_cleanup(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archive-http-") as directory:
            service = ArchiveService(
                directory,
                bootstrap_regular_days=0,
                bootstrap_audit_days=0,
                bootstrap_legal_days=0,
            )
            server = make_server(service, port=0, quiet=True)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                host, port = server.server_address[:2]
                connection = http.client.HTTPConnection(host, port, timeout=10)
                connection.request(
                    "PUT",
                    "/api/v1/objects?key=http%2Freport&category=audit",
                    body=b"http-content",
                    headers={"Content-Type": "application/octet-stream"},
                )
                response = connection.getresponse()
                self.assertEqual(response.status, 201)
                written = json.loads(response.read())
                version_id = written["version_id"]
                connection.request("GET", f"/api/v1/versions/{version_id}")
                response = connection.getresponse()
                self.assertEqual(response.status, 200)
                self.assertEqual(response.read(), b"http-content")
                connection.request(
                    "GET",
                    "/api/v1/objects/read?key=http%2Freport&version=1",
                )
                response = connection.getresponse()
                self.assertEqual(response.status, 200)
                self.assertEqual(response.read(), b"http-content")
                connection.request(
                    "POST",
                    "/api/v1/cleanup",
                    body=b"{}",
                    headers={"Content-Type": "application/json"},
                )
                response = connection.getresponse()
                cleanup = json.loads(response.read())
                self.assertEqual(response.status, 200)
                self.assertEqual(cleanup["deleted_version_count"], 1)
                connection.request("GET", f"/api/v1/versions/{version_id}")
                response = connection.getresponse()
                self.assertEqual(response.status, 410)
                response.read()
                connection.close()
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)


if __name__ == "__main__":
    unittest.main()
