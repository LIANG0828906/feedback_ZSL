from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
import uuid
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable

from .db import Database
from .errors import ApiError
from .util import (
    add_days,
    copy_file_atomic,
    format_time,
    hash_file,
    parse_time,
    remove_quietly,
    utc_now,
)


DOMAINS = ("primary", "replica-a", "replica-b")
CATEGORIES = ("regular", "audit", "legal")


def _json_dump(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def _json_load(value: str | None, default: Any) -> Any:
    if value is None:
        return default
    try:
        return json.loads(value)
    except json.JSONDecodeError:
        return default


class ArchiveService:
    """Compliant three-domain archive with an independent lock table.

    Object metadata and lock records are deliberately stored separately.  Delete
    admission always resolves their retention deadlines by taking the later
    value, which is the safety-critical rule exercised by the incident.
    """

    def __init__(
        self,
        data_dir: str | Path,
        *,
        bootstrap_regular_days: int = 180,
        bootstrap_audit_days: int = 180,
        bootstrap_legal_days: int = 180,
        max_object_bytes: int = 20 * 1024 * 1024,
        bootstrap_effective_at: str | None = None,
    ):
        self.data_dir = Path(data_dir).resolve()
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.domains_root = self.data_dir / "domains"
        self.tmp_dir = self.data_dir / "tmp"
        self.tmp_dir.mkdir(parents=True, exist_ok=True)
        for domain in DOMAINS:
            (self.domains_root / domain).mkdir(parents=True, exist_ok=True)
        self.database = Database(self.data_dir / "archive.db")
        self.database.initialize(
            regular_days=bootstrap_regular_days,
            audit_days=bootstrap_audit_days,
            legal_days=bootstrap_legal_days,
            effective_at=bootstrap_effective_at,
        )
        self.max_object_bytes = int(max_object_bytes)
        self._cleanup_mutex = threading.Lock()
        self._delete_mutex = threading.RLock()
        self._reconcile_mutex = threading.Lock()
        self._recover_interrupted_cleanup()
        self.activate_due_policies()

    # ------------------------------------------------------------------
    # Small helpers
    # ------------------------------------------------------------------

    @staticmethod
    def normalize_category(value: str | None) -> str:
        category = (value or "regular").strip().lower().replace("-", "_")
        aliases = {
            "normal": "regular",
            "standard": "regular",
            "法定封存": "legal",
            "legal_hold": "legal",
            "sealed": "legal",
        }
        category = aliases.get(category, category)
        if category not in CATEGORIES:
            raise ApiError(
                "INVALID_RETENTION_CATEGORY",
                400,
                "retention category must be regular, audit or legal",
                {"allowed": list(CATEGORIES), "received": value},
            )
        return category

    @staticmethod
    def parse_bool(value: Any, *, default: bool = False) -> bool:
        if value is None:
            return default
        if isinstance(value, bool):
            return value
        text = str(value).strip().lower()
        if text in {"1", "true", "yes", "on"}:
            return True
        if text in {"0", "false", "no", "off", ""}:
            return False
        raise ApiError(
            "INVALID_BOOLEAN",
            400,
            "expected a boolean value",
            {"received": value},
        )

    @staticmethod
    def _parse_time_argument(
        value: str | datetime | None,
        *,
        field: str,
        default: datetime | None = None,
    ) -> datetime:
        try:
            return parse_time(value, default=default)
        except (TypeError, ValueError) as exc:
            raise ApiError(
                "INVALID_TIMESTAMP",
                400,
                f"{field} must be an ISO-8601 timestamp",
                {"field": field, "received": value, "detail": str(exc)},
            )

    @staticmethod
    def normalize_domains(values: Iterable[str] | None) -> set[str]:
        result: set[str] = set()
        for value in values or ():
            for item in str(value).split(","):
                item = item.strip()
                if not item:
                    continue
                if item not in DOMAINS:
                    raise ApiError(
                        "UNKNOWN_STORAGE_DOMAIN",
                        400,
                        "unknown storage domain",
                        {"allowed": list(DOMAINS), "received": item},
                    )
                result.add(item)
        return result

    def _domain_path(self, domain: str, version_id: str) -> Path:
        if (
            domain not in DOMAINS
            or len(version_id) != 32
            or any(character not in "0123456789abcdef" for character in version_id)
        ):
            raise ApiError(
                "INVALID_DOMAIN_PATH",
                400,
                "domain or version identifier is invalid",
                {"domain": domain, "version_id": version_id},
            )
        shard = version_id[:2]
        return self.domains_root / domain / shard / f"{version_id}.bin"

    def new_temp_path(self, prefix: str = "request") -> Path:
        return self.tmp_dir / f"{prefix}-{uuid.uuid4().hex}.part"

    def _recover_interrupted_cleanup(self) -> None:
        with self.database.transaction(immediate=True) as connection:
            connection.execute(
                "UPDATE object_versions SET status = 'cleanup_partial' "
                "WHERE status = 'cleaning'"
            )

    # ------------------------------------------------------------------
    # Policies
    # ------------------------------------------------------------------

    @staticmethod
    def _policy_values(connection: sqlite3.Connection, publication_id: int) -> dict[str, int]:
        rows = connection.execute(
            "SELECT category, retention_days FROM policy_values WHERE publication_id = ?",
            (publication_id,),
        ).fetchall()
        values = {row["category"]: int(row["retention_days"]) for row in rows}
        return {
            "regular": values.get("regular", 180),
            "audit": values.get("audit", 180),
            "legal": values.get("legal", 180),
        }

    def _policy_at(
        self,
        connection: sqlite3.Connection,
        at: datetime,
    ) -> sqlite3.Row | None:
        return connection.execute(
            "SELECT * FROM policy_publications "
            "WHERE effective_at <= ? "
            "ORDER BY effective_at DESC, publication_id DESC LIMIT 1",
            (format_time(at),),
        ).fetchone()

    def _latest_policy(self, connection: sqlite3.Connection) -> sqlite3.Row | None:
        return connection.execute(
            "SELECT * FROM policy_publications "
            "ORDER BY effective_at DESC, publication_id DESC LIMIT 1"
        ).fetchone()

    def _policy_publication_json(
        self,
        connection: sqlite3.Connection,
        row: sqlite3.Row,
    ) -> dict[str, Any]:
        values = self._policy_values(connection, int(row["publication_id"]))
        result: dict[str, Any] = {
            "applied_at": row["applied_at"],
            "created_at": row["created_at"],
            "effective_at": row["effective_at"],
            "legal_days": values["legal"],
            "legal_hold_days": values["legal"],
            "publication_id": int(row["publication_id"]),
            "retention_days": values,
            "values": values,
        }
        result.update(
            {
                "regular_days": values["regular"],
                "audit_days": values["audit"],
            }
        )
        return result

    def current_policy(self) -> dict[str, Any]:
        now = utc_now()
        with self.database.transaction(immediate=True) as connection:
            self._activate_due_policies(connection, now)
            active = self._policy_at(connection, now)
            latest = self._latest_policy(connection)
            if active is None:
                raise ApiError("POLICY_NOT_INITIALIZED", 500, "no effective retention policy")
            history = [
                self._policy_publication_json(connection, row)
                for row in connection.execute(
                    "SELECT * FROM policy_publications "
                    "ORDER BY publication_id DESC LIMIT 50"
                ).fetchall()
            ]
            active_json = self._policy_publication_json(connection, active)
            latest_json = (
                self._policy_publication_json(connection, latest) if latest is not None else None
            )
        return {
            "active": active_json,
            "history": history,
            "latest_published": latest_json,
            "server_time": format_time(now),
        }

    def publish_policy(
        self,
        *,
        regular_days: int,
        audit_days: int,
        legal_days: int,
        effective_at: str | datetime | None = None,
    ) -> dict[str, Any]:
        proposed = {
            "regular": self._validate_days("regular", regular_days),
            "audit": self._validate_days("audit", audit_days),
            "legal": self._validate_days("legal", legal_days),
        }
        now = utc_now()
        effective = self._parse_time_argument(
            effective_at, field="effective_at", default=now
        )
        with self.database.transaction(immediate=True) as connection:
            baseline_row = self._policy_at(connection, now)
            if baseline_row is None:
                baseline_row = self._latest_policy(connection)
            if baseline_row is not None:
                baseline = self._policy_values(
                    connection, int(baseline_row["publication_id"])
                )
                shortened = {
                    category: {
                        "current": baseline[category],
                        "proposed": proposed[category],
                    }
                    for category in CATEGORIES
                    if proposed[category] < baseline[category]
                }
                if shortened:
                    raise ApiError(
                        "RETENTION_POLICY_SHORTENING_REJECTED",
                        409,
                        "a retention class may not be shortened",
                        {
                            "effective_at": baseline_row["effective_at"],
                            "publication_id": int(baseline_row["publication_id"]),
                            "shortened_categories": shortened,
                        },
                    )
            created_at = format_time(now)
            cursor = connection.execute(
                "INSERT INTO policy_publications(effective_at, created_at, applied_at) "
                "VALUES (?, ?, NULL)",
                (format_time(effective), created_at),
            )
            publication_id = int(cursor.lastrowid)
            connection.executemany(
                "INSERT INTO policy_values(publication_id, category, retention_days) "
                "VALUES (?, ?, ?)",
                [
                    (publication_id, category, proposed[category])
                    for category in CATEGORIES
                ],
            )
            changed_version_ids: list[str] = []
            if effective <= now:
                changed_version_ids = self._activate_due_policies(connection, now)
            row = connection.execute(
                "SELECT * FROM policy_publications WHERE publication_id = ?",
                (publication_id,),
            ).fetchone()
            response = self._policy_publication_json(connection, row)
            response["changed_version_count"] = len(changed_version_ids)
            response["changed_version_ids"] = sorted(changed_version_ids)
        return response

    @staticmethod
    def _validate_days(category: str, value: Any) -> int:
        try:
            days = int(value)
        except (TypeError, ValueError):
            raise ApiError(
                "INVALID_RETENTION_DAYS",
                400,
                "retention days must be an integer",
                {"category": category, "received": value},
            )
        if days < 0 or days > 36500:
            raise ApiError(
                "INVALID_RETENTION_DAYS",
                400,
                "retention days must be between 0 and 36500",
                {"category": category, "received": value},
            )
        return days

    def _activate_due_policies(
        self,
        connection: sqlite3.Connection,
        now: datetime | None = None,
    ) -> list[str]:
        now = now or utc_now()
        publications = connection.execute(
            "SELECT * FROM policy_publications "
            "WHERE applied_at IS NULL AND effective_at <= ? "
            "ORDER BY effective_at ASC, publication_id ASC",
            (format_time(now),),
        ).fetchall()
        changed: list[str] = []
        for publication in publications:
            values = self._policy_values(connection, int(publication["publication_id"]))
            versions = connection.execute(
                "SELECT version_id, written_at, category, legal_hold, retention_until "
                "FROM object_versions WHERE status != 'deleted'"
            ).fetchall()
            for version in versions:
                written_at = parse_time(version["written_at"])
                category = str(version["category"])
                candidate = add_days(written_at, values.get(category, values["regular"]))
                if int(version["legal_hold"]):
                    candidate = max(candidate, add_days(written_at, values["legal"]))
                candidate_text = format_time(candidate)
                old_metadata = str(version["retention_until"])
                new_metadata = old_metadata
                if candidate_text > version["retention_until"]:
                    connection.execute(
                        "UPDATE object_versions SET retention_until = ? "
                        "WHERE version_id = ?",
                        (candidate_text, version["version_id"]),
                    )
                    new_metadata = candidate_text
                lock = connection.execute(
                    "SELECT retention_until, lock_pinned FROM lock_records WHERE version_id = ?",
                    (version["version_id"],),
                ).fetchone()
                old_lock = lock["retention_until"] if lock is not None else None
                new_lock = old_lock
                if lock is not None and not int(lock["lock_pinned"]):
                    if candidate_text > lock["retention_until"]:
                        connection.execute(
                            "UPDATE lock_records SET retention_until = ?, updated_at = ? "
                            "WHERE version_id = ?",
                            (
                                candidate_text,
                                format_time(now),
                                version["version_id"],
                            ),
                        )
                        new_lock = candidate_text
                if new_metadata != old_metadata or new_lock != old_lock:
                    changed.append(str(version["version_id"]))
                    connection.execute(
                        "INSERT OR REPLACE INTO policy_extension_events("
                        "publication_id, version_id, old_metadata_until, "
                        "new_metadata_until, old_lock_until, new_lock_until, created_at"
                        ") VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (
                            publication["publication_id"],
                            version["version_id"],
                            old_metadata,
                            new_metadata,
                            old_lock,
                            new_lock,
                            format_time(now),
                        ),
                    )
            connection.execute(
                "UPDATE policy_publications SET applied_at = ? WHERE publication_id = ?",
                (format_time(now), publication["publication_id"]),
            )
        return changed

    def activate_due_policies(self) -> list[str]:
        with self.database.transaction(immediate=True) as connection:
            return self._activate_due_policies(connection, utc_now())

    def _ensure_due_policies(self) -> None:
        with self.database.transaction() as connection:
            due = connection.execute(
                "SELECT 1 FROM policy_publications "
                "WHERE applied_at IS NULL AND effective_at <= ? LIMIT 1",
                (format_time(utc_now()),),
            ).fetchone()
        if due is not None:
            self.activate_due_policies()

    # ------------------------------------------------------------------
    # Object writes and reads
    # ------------------------------------------------------------------

    def write_object(
        self,
        *,
        object_key: str,
        source_path: str | Path,
        content_length: int,
        content_sha256: str,
        category: str = "regular",
        legal_hold: bool = False,
        written_at: str | datetime | None = None,
        fail_domains: Iterable[str] | None = None,
    ) -> tuple[dict[str, Any], int]:
        object_key = str(object_key or "").strip()
        if not object_key:
            raise ApiError("OBJECT_KEY_REQUIRED", 400, "object_key is required")
        if len(object_key) > 1024:
            raise ApiError(
                "OBJECT_KEY_TOO_LONG",
                400,
                "object_key must be at most 1024 characters",
            )
        category = self.normalize_category(category)
        failures = self.normalize_domains(fail_domains)
        if content_length > self.max_object_bytes:
            raise ApiError(
                "OBJECT_TOO_LARGE",
                413,
                "object exceeds max_object_bytes",
                {"limit": self.max_object_bytes, "received": content_length},
            )
        version_id = uuid.uuid4().hex
        written = self._parse_time_argument(
            written_at, field="written_at", default=utc_now()
        )
        domain_results: dict[str, dict[str, Any]] = {}
        successful_domains: list[str] = []
        try:
            for domain in DOMAINS:
                if domain in failures:
                    domain_results[domain] = {
                        "error": "SIMULATED_WRITE_FAILURE",
                        "length": None,
                        "sha256": None,
                        "status": "missing",
                    }
                    continue
                try:
                    path = self._domain_path(domain, version_id)
                    copied_length, copied_hash = copy_file_atomic(source_path, path)
                    if copied_length != content_length or copied_hash != content_sha256:
                        raise OSError("source changed while writing")
                    successful_domains.append(domain)
                    domain_results[domain] = {
                        "error": None,
                        "length": copied_length,
                        "sha256": copied_hash,
                        "status": "written",
                    }
                except (OSError, ValueError) as exc:
                    domain_results[domain] = {
                        "error": f"{type(exc).__name__}: {exc}",
                        "length": None,
                        "sha256": None,
                        "status": "missing",
                    }

            complete = len(successful_domains) == len(DOMAINS)
            status = "complete" if complete else "partial"
            now = utc_now()
            row_result: dict[str, Any]
            try:
                with self.database.transaction(immediate=True) as connection:
                    self._activate_due_policies(connection, now)
                    active = self._policy_at(
                        connection,
                        written if written_at is not None else now,
                    )
                    if active is None:
                        active = self._policy_at(connection, now)
                    if active is None:
                        raise ApiError(
                            "POLICY_NOT_INITIALIZED", 500, "no effective retention policy"
                        )
                    values = self._policy_values(connection, int(active["publication_id"]))
                    retention = add_days(written, values[category])
                    if legal_hold:
                        retention = max(retention, add_days(written, values["legal"]))
                    retention_text = format_time(retention)
                    next_version = int(
                        connection.execute(
                            "SELECT COALESCE(MAX(version_number), 0) + 1 "
                            "FROM object_versions WHERE object_key = ?",
                            (object_key,),
                        ).fetchone()[0]
                    )
                    created_at = format_time(now)
                    connection.execute(
                        "INSERT INTO object_versions("
                        "version_id, object_key, version_number, written_at, category, "
                        "legal_hold, retention_until, content_length, content_sha256, "
                        "status, created_at"
                        ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                            version_id,
                            object_key,
                            next_version,
                            format_time(written),
                            category,
                            1 if legal_hold else 0,
                            retention_text,
                            content_length,
                            content_sha256,
                            status,
                            created_at,
                        ),
                    )
                    for domain in DOMAINS:
                        result = domain_results[domain]
                        connection.execute(
                            "INSERT INTO domain_copies("
                            "version_id, domain, write_status, expected_length, "
                            "expected_sha256, delete_status, last_error"
                            ") VALUES (?, ?, ?, ?, ?, ?, ?)",
                            (
                                version_id,
                                domain,
                                result["status"],
                                content_length,
                                content_sha256,
                                "present" if result["status"] == "written" else "not_written",
                                result["error"],
                            ),
                        )
                    connection.execute(
                        "INSERT INTO lock_records("
                        "version_id, retention_until, lock_pinned, updated_at"
                        ") VALUES (?, ?, 0, ?)",
                        (version_id, retention_text, created_at),
                    )
                    row_result = {
                        "category": category,
                        "complete": complete,
                        "content_length": content_length,
                        "content_sha256": content_sha256,
                        "domains": [
                            {
                                "domain": domain,
                                **domain_results[domain],
                            }
                            for domain in DOMAINS
                        ],
                        "earliest_deletable_at": (
                            None if legal_hold else retention_text
                        ),
                        "legal_hold": bool(legal_hold),
                        "missing_domains": [
                            domain
                            for domain in DOMAINS
                            if domain_results[domain]["status"] != "written"
                        ],
                        "object_key": object_key,
                        "retention_until": retention_text,
                        "status": status,
                        "version_id": version_id,
                        "version_number": next_version,
                        "written_at": format_time(written),
                    }
            except BaseException:
                for domain in successful_domains:
                    remove_quietly(self._domain_path(domain, version_id))
                raise
            return row_result, (201 if complete else 502)
        finally:
            pass

    def _version_row(
        self,
        connection: sqlite3.Connection,
        version_id: str,
    ) -> sqlite3.Row:
        row = connection.execute(
            "SELECT * FROM object_versions WHERE version_id = ?",
            (version_id,),
        ).fetchone()
        if row is None:
            raise ApiError(
                "OBJECT_VERSION_NOT_FOUND",
                404,
                "object version does not exist",
                {"version_id": version_id},
            )
        return row

    def version_metadata(self, version_id: str) -> dict[str, Any]:
        self._ensure_due_policies()
        with self.database.transaction() as connection:
            row = self._version_row(connection, version_id)
            domains = connection.execute(
                "SELECT * FROM domain_copies WHERE version_id = ? "
                "ORDER BY CASE domain "
                "WHEN 'primary' THEN 0 WHEN 'replica-a' THEN 1 ELSE 2 END",
                (version_id,),
            ).fetchall()
            lock = connection.execute(
                "SELECT * FROM lock_records WHERE version_id = ?",
                (version_id,),
            ).fetchone()
            return self._serialize_version(row, domains, lock)

    @staticmethod
    def _serialize_version(
        row: sqlite3.Row,
        domains: Iterable[sqlite3.Row],
        lock: sqlite3.Row | None,
    ) -> dict[str, Any]:
        domain_values = [
            {
                "delete_status": item["delete_status"],
                "domain": item["domain"],
                "expected_length": int(item["expected_length"]),
                "expected_sha256": item["expected_sha256"],
                "last_error": item["last_error"],
                "write_status": item["write_status"],
            }
            for item in domains
        ]
        return {
            "category": row["category"],
            "content_length": int(row["content_length"]),
            "content_sha256": row["content_sha256"],
            "created_at": row["created_at"],
            "deleted_at": row["deleted_at"],
            "domains": domain_values,
            "legal_hold": bool(row["legal_hold"]),
            "lock": (
                {
                    "lock_pinned": bool(lock["lock_pinned"]),
                    "retention_until": lock["retention_until"],
                    "updated_at": lock["updated_at"],
                }
                if lock is not None
                else None
            ),
            "object_key": row["object_key"],
            "retention_until": row["retention_until"],
            "status": row["status"],
            "version_id": row["version_id"],
            "version_number": int(row["version_number"]),
            "written_at": row["written_at"],
        }

    def resolve_version_id(
        self,
        *,
        object_key: str,
        version: str | int,
    ) -> str:
        key = str(object_key or "").strip()
        if not key:
            raise ApiError("OBJECT_KEY_REQUIRED", 400, "object_key is required")
        with self.database.transaction() as connection:
            version_text = str(version)
            if (
                len(version_text) == 32
                and all(
                    character in "0123456789abcdef" for character in version_text
                )
            ):
                row = connection.execute(
                    "SELECT version_id FROM object_versions "
                    "WHERE version_id = ? AND object_key = ?",
                    (version_text, key),
                ).fetchone()
            else:
                try:
                    number = int(version_text)
                except (TypeError, ValueError):
                    raise ApiError(
                        "OBJECT_VERSION_NOT_FOUND",
                        404,
                        "object version does not exist",
                        {"object_key": key, "version": version},
                    )
                row = connection.execute(
                    "SELECT version_id FROM object_versions "
                    "WHERE object_key = ? AND version_number = ?",
                    (key, number),
                ).fetchone()
            if row is None:
                raise ApiError(
                    "OBJECT_VERSION_NOT_FOUND",
                    404,
                    "object version does not exist",
                    {"object_key": key, "version": version},
                )
            return str(row["version_id"])

    def locate_readable_copy(self, version_id: str) -> tuple[Path, dict[str, Any]]:
        self._ensure_due_policies()
        with self.database.transaction() as connection:
            row = self._version_row(connection, version_id)
            if row["status"] == "deleted":
                raise ApiError(
                    "OBJECT_VERSION_DELETED",
                    410,
                    "object version has been deleted",
                    {"version_id": version_id},
                )
            domains = connection.execute(
                "SELECT * FROM domain_copies WHERE version_id = ? "
                "ORDER BY CASE domain "
                "WHEN 'primary' THEN 0 WHEN 'replica-a' THEN 1 ELSE 2 END",
                (version_id,),
            ).fetchall()
            for domain in domains:
                if domain["write_status"] != "written":
                    continue
                if domain["delete_status"] == "deleted":
                    continue
                path = self._domain_path(domain["domain"], version_id)
                if path.is_file():
                    stat = path.stat()
                    if stat.st_size != int(domain["expected_length"]):
                        continue
                    return path, {
                        "content_length": int(domain["expected_length"]),
                        "content_sha256": domain["expected_sha256"],
                        "domain": domain["domain"],
                        "legal_hold": bool(row["legal_hold"]),
                        "retention_until": row["retention_until"],
                        "version_id": version_id,
                    }
            raise ApiError(
                "OBJECT_CONTENT_UNAVAILABLE",
                502,
                "no readable domain copy is available",
                {"version_id": version_id},
            )

    def read_version(self, version_id: str) -> tuple[Path, dict[str, Any]]:
        return self.locate_readable_copy(version_id)

    # ------------------------------------------------------------------
    # Delete admission, domain deletion and cleanup resume
    # ------------------------------------------------------------------

    @staticmethod
    def _admission_values(
        *,
        version_id: str,
        object_key: str,
        version_number: int,
        status: str,
        legal_hold: bool,
        metadata_retention_until: str | None,
        lock_retention_until: str | None,
        now: datetime,
    ) -> dict[str, Any]:
        now_text = format_time(now)
        reason_codes: list[str] = []
        allowed = False
        reason = ""
        conflict = metadata_retention_until != lock_retention_until
        effective = metadata_retention_until
        if conflict and metadata_retention_until and lock_retention_until:
            effective = max(metadata_retention_until, lock_retention_until)
            reason_codes.append("RETENTION_DEADLINE_MISMATCH")
        elif metadata_retention_until is None:
            effective = lock_retention_until

        if status == "deleted":
            reason = "VERSION_ALREADY_DELETED"
        elif status == "cleaning":
            reason = "CLEANUP_IN_PROGRESS"
        elif legal_hold:
            reason = "LEGAL_HOLD_ACTIVE"
        elif lock_retention_until is None:
            reason = "LOCK_RECORD_MISSING"
        elif effective is None:
            reason = "RETENTION_UNKNOWN"
        elif effective > now_text:
            reason = "RETENTION_CONFLICT_ACTIVE" if conflict else "RETENTION_ACTIVE"
        else:
            allowed = True
            reason = "ELIGIBLE"

        basis = (
            "max(object_versions.retention_until, lock_records.retention_until)"
            if conflict
            else "object_versions.retention_until"
        )
        if legal_hold:
            basis = "legal_hold=true (unconditional denial)"
        if reason and reason not in reason_codes:
            reason_codes.insert(0, reason)
        return {
            "allowed": allowed,
            "conflict": {
                "fields": (
                    [
                        "object_versions.retention_until",
                        "lock_records.retention_until",
                    ]
                    if conflict
                    else []
                ),
                "lock_retention_until": lock_retention_until,
                "metadata_retention_until": metadata_retention_until,
                "resolution": "take_later" if conflict else "single_source",
                "resolved_retention_until": effective,
            },
            "decision_basis": basis,
            "evaluated_at": now_text,
            "inconsistency_reason_code": (
                "RETENTION_DEADLINE_MISMATCH" if conflict else None
            ),
            "legal_hold": bool(legal_hold),
            "object_key": object_key,
            "reason_code": reason,
            "reason_codes": reason_codes,
            "retention_resolution": {
                "effective_retention_until": effective,
                "lock_retention_until": lock_retention_until,
                "metadata_retention_until": metadata_retention_until,
                "policy": "maximum",
            },
            "status": status,
            "version_id": version_id,
            "version_number": int(version_number),
        }

    def admission(self, version_id: str, *, record_denial: bool = False) -> dict[str, Any]:
        now = utc_now()
        with self.database.transaction(immediate=True) as connection:
            self._activate_due_policies(connection, now)
            row = self._version_row(connection, version_id)
            lock = connection.execute(
                "SELECT * FROM lock_records WHERE version_id = ?",
                (version_id,),
            ).fetchone()
            result = self._admission_values(
                version_id=str(row["version_id"]),
                object_key=str(row["object_key"]),
                version_number=int(row["version_number"]),
                status=str(row["status"]),
                legal_hold=bool(row["legal_hold"]),
                metadata_retention_until=row["retention_until"],
                lock_retention_until=(
                    lock["retention_until"] if lock is not None else None
                ),
                now=now,
            )
            if (
                record_denial
                and not result["allowed"]
                and result["reason_code"] == "RETENTION_CONFLICT_ACTIVE"
            ):
                self._increment_counter(connection, "retention_conflict_delete_denials")
                result["denial_counted"] = True
            elif record_denial and not result["allowed"]:
                result["denial_counted"] = False
            return result

    @staticmethod
    def _increment_counter(connection: sqlite3.Connection, name: str, amount: int = 1) -> None:
        connection.execute(
            "INSERT INTO counters(name, value) VALUES (?, ?) "
            "ON CONFLICT(name) DO UPDATE SET value = value + excluded.value",
            (name, int(amount)),
        )

    def _mark_cleaning(self, version_id: str) -> None:
        with self.database.transaction(immediate=True) as connection:
            connection.execute(
                "UPDATE object_versions SET status = 'cleaning' "
                "WHERE version_id = ? AND status IN ('complete', 'partial', 'cleanup_partial')",
                (version_id,),
            )

    def delete_version(
        self,
        version_id: str,
        *,
        fail_domains: Iterable[str] | None = None,
        _cleanup_mode: bool = False,
    ) -> dict[str, Any]:
        failures = self.normalize_domains(fail_domains)
        with self._delete_mutex:
            admission = self.admission(version_id, record_denial=not _cleanup_mode)
            if not admission["allowed"]:
                raise ApiError(
                    admission["reason_code"],
                    409,
                    "delete admission rejected",
                    {"admission": admission},
                )
            self._mark_cleaning(version_id)
            with self.database.transaction() as connection:
                domains = connection.execute(
                    "SELECT * FROM domain_copies WHERE version_id = ? "
                    "ORDER BY CASE domain "
                    "WHEN 'primary' THEN 0 WHEN 'replica-a' THEN 1 ELSE 2 END",
                    (version_id,),
                ).fetchall()
            deleted_domains: list[str] = []
            failed_domains: list[str] = []
            for domain_row in domains:
                domain = str(domain_row["domain"])
                if domain_row["write_status"] != "written":
                    continue
                if domain_row["delete_status"] == "deleted":
                    continue
                if domain in failures:
                    failed_domains.append(domain)
                    with self.database.transaction(immediate=True) as connection:
                        connection.execute(
                            "UPDATE domain_copies SET delete_status = 'failed', "
                            "last_error = ? WHERE version_id = ? AND domain = ?",
                            ("SIMULATED_DELETE_FAILURE", version_id, domain),
                        )
                    break
                path = self._domain_path(domain, version_id)
                try:
                    path.unlink()
                except FileNotFoundError:
                    pass
                except OSError as exc:
                    failed_domains.append(domain)
                    with self.database.transaction(immediate=True) as connection:
                        connection.execute(
                            "UPDATE domain_copies SET delete_status = 'failed', "
                            "last_error = ? WHERE version_id = ? AND domain = ?",
                            (f"{type(exc).__name__}: {exc}", version_id, domain),
                        )
                    break
                deleted_domains.append(domain)
                with self.database.transaction(immediate=True) as connection:
                    connection.execute(
                        "UPDATE domain_copies SET delete_status = 'deleted', "
                        "last_error = NULL WHERE version_id = ? AND domain = ?",
                        (version_id, domain),
                    )

            with self.database.transaction(immediate=True) as connection:
                final_domains = connection.execute(
                    "SELECT * FROM domain_copies WHERE version_id = ? "
                    "ORDER BY CASE domain "
                    "WHEN 'primary' THEN 0 WHEN 'replica-a' THEN 1 ELSE 2 END",
                    (version_id,),
                ).fetchall()
                unfinished = [
                    row
                    for row in final_domains
                    if row["write_status"] == "written" and row["delete_status"] != "deleted"
                ]
                now_text = format_time(utc_now())
                if unfinished:
                    connection.execute(
                        "UPDATE object_versions SET status = 'cleanup_partial' "
                        "WHERE version_id = ?",
                        (version_id,),
                    )
                    connection.execute(
                        "UPDATE lock_records SET updated_at = ? WHERE version_id = ?",
                        (now_text, version_id),
                    )
                    complete = False
                else:
                    connection.execute(
                        "UPDATE object_versions SET status = 'deleted', deleted_at = ? "
                        "WHERE version_id = ?",
                        (now_text, version_id),
                    )
                    connection.execute(
                        "DELETE FROM lock_records WHERE version_id = ?",
                        (version_id,),
                    )
                    complete = True
                lock = connection.execute(
                    "SELECT * FROM lock_records WHERE version_id = ?",
                    (version_id,),
                ).fetchone()
                row = self._version_row(connection, version_id)
                serialized = self._serialize_version(row, final_domains, lock)
            return {
                "admission": admission,
                "complete": complete,
                "deleted_domains": deleted_domains,
                "failed_domains": failed_domains,
                "lock_record_present": not complete,
                "version": serialized,
            }

    def _candidate_versions(
        self,
        connection: sqlite3.Connection,
        now: datetime,
    ) -> tuple[list[dict[str, Any]], int]:
        rows = connection.execute(
            "SELECT v.*, l.retention_until AS lock_retention_until "
            "FROM object_versions v LEFT JOIN lock_records l "
            "ON l.version_id = v.version_id "
            "WHERE v.status IN ('complete', 'partial', 'cleanup_partial') "
            "ORDER BY v.retention_until ASC, v.object_key ASC, v.version_number ASC"
        ).fetchall()
        candidates: list[dict[str, Any]] = []
        retained = 0
        for row in rows:
            metadata_until = row["retention_until"]
            lock_until = row["lock_retention_until"]
            effective = (
                max(metadata_until, lock_until)
                if metadata_until and lock_until
                else metadata_until or lock_until
            )
            if (
                row["legal_hold"]
                or lock_until is None
                or effective is None
                or effective > format_time(now)
            ):
                retained += 1
                continue
            candidates.append(
                {
                    "effective_retention_until": effective,
                    "object_key": row["object_key"],
                    "status": row["status"],
                    "version_id": row["version_id"],
                    "version_number": int(row["version_number"]),
                }
            )
        return candidates, retained

    def cleanup(
        self,
        *,
        fail_domains: Iterable[str] | None = None,
        dry_run: bool = False,
    ) -> dict[str, Any]:
        failures = self.normalize_domains(fail_domains)
        if not self._cleanup_mutex.acquire(blocking=False):
            raise ApiError(
                "CLEANUP_ALREADY_RUNNING",
                409,
                "another cleanup run is already in progress",
            )
        try:
            cleanup_id = uuid.uuid4().hex
            started = utc_now()
            now = started
            with self.database.transaction(immediate=True) as connection:
                self._activate_due_policies(connection, now)
                candidates, retained_count = self._candidate_versions(connection, now)
            deleted_count = 0
            failed_count = 0
            processed: list[dict[str, Any]] = []
            by_domain = {domain: 0 for domain in DOMAINS}
            failed_domains: list[str] = []
            if not dry_run:
                with self._delete_mutex:
                    for candidate in candidates:
                        version_id = str(candidate["version_id"])
                        try:
                            outcome = self.delete_version(
                                version_id,
                                fail_domains=failures,
                                _cleanup_mode=True,
                            )
                        except ApiError as exc:
                            if exc.code in {
                                "RETENTION_ACTIVE",
                                "RETENTION_CONFLICT_ACTIVE",
                                "LEGAL_HOLD_ACTIVE",
                                "CLEANUP_IN_PROGRESS",
                                "VERSION_ALREADY_DELETED",
                            }:
                                retained_count += 1
                                continue
                            raise
                        if outcome["complete"]:
                            deleted_count += 1
                        else:
                            failed_count += 1
                            failed_domains.extend(outcome["failed_domains"])
                        for domain in outcome["deleted_domains"]:
                            by_domain[domain] += 1
                        processed.append(
                            {
                                "complete": outcome["complete"],
                                "deleted_domains": outcome["deleted_domains"],
                                "failed_domains": outcome["failed_domains"],
                                "version_id": version_id,
                            }
                        )
            with self.database.transaction() as connection:
                post_candidates, _ = self._candidate_versions(connection, utc_now())
            finished = utc_now()
            summary = {
                "cleanup_id": cleanup_id,
                "deleted_by_domain": by_domain,
                "deleted_version_count": deleted_count,
                "dry_run": bool(dry_run),
                "eligible_version_count": len(candidates),
                "failed_domains": sorted(set(failed_domains)),
                "failed_version_count": failed_count,
                "finished_at": format_time(finished),
                "pending_version_count": len(post_candidates),
                "processed_versions": processed,
                "retained_version_count": retained_count,
                "started_at": format_time(started),
                "status": (
                    "dry_run"
                    if dry_run
                    else ("partial" if failed_count else "completed")
                ),
            }
            self._state_set("last_cleanup", summary)
            return summary
        finally:
            self._cleanup_mutex.release()

    # ------------------------------------------------------------------
    # Cross-domain reconciliation
    # ------------------------------------------------------------------

    def reconcile(self) -> dict[str, Any]:
        if not self._reconcile_mutex.acquire(blocking=False):
            raise ApiError(
                "RECONCILIATION_ALREADY_RUNNING",
                409,
                "another reconciliation is already in progress",
            )
        try:
            reconciliation_id = uuid.uuid4().hex
            started = utc_now()
            with self.database.transaction() as connection:
                versions = connection.execute(
                    "SELECT v.* FROM object_versions v "
                    "WHERE v.status = 'complete' "
                    "AND (SELECT COUNT(*) FROM domain_copies d "
                    "     WHERE d.version_id = v.version_id "
                    "     AND d.write_status = 'written' "
                    "     AND d.delete_status != 'deleted') = 3 "
                    "ORDER BY v.object_key ASC, v.version_number ASC, v.version_id ASC"
                ).fetchall()
                snapshots: list[dict[str, Any]] = []
                for version in versions:
                    domains = connection.execute(
                        "SELECT * FROM domain_copies WHERE version_id = ? "
                        "ORDER BY CASE domain "
                        "WHEN 'primary' THEN 0 WHEN 'replica-a' THEN 1 ELSE 2 END",
                        (version["version_id"],),
                    ).fetchall()
                    snapshots.append(
                        {
                            "domains": [dict(row) for row in domains],
                            "object_key": version["object_key"],
                            "version_id": version["version_id"],
                            "version_number": int(version["version_number"]),
                        }
                    )

            differences: list[dict[str, Any]] = []
            checked = 0
            for snapshot in snapshots:
                for expected in snapshot["domains"]:
                    checked += 1
                    domain = str(expected["domain"])
                    expected_length = int(expected["expected_length"])
                    expected_sha = str(expected["expected_sha256"])
                    path = self._domain_path(domain, str(snapshot["version_id"]))
                    try:
                        actual_length, actual_sha = hash_file(path)
                    except FileNotFoundError:
                        differences.append(
                            self._difference_entry(
                                snapshot,
                                domain,
                                "missing",
                                expected_length,
                                expected_sha,
                                None,
                                None,
                            )
                        )
                        continue
                    except OSError:
                        differences.append(
                            self._difference_entry(
                                snapshot,
                                domain,
                                "missing",
                                expected_length,
                                expected_sha,
                                None,
                                None,
                            )
                        )
                        continue
                    if actual_length != expected_length:
                        differences.append(
                            self._difference_entry(
                                snapshot,
                                domain,
                                "length_mismatch",
                                expected_length,
                                expected_sha,
                                actual_length,
                                actual_sha,
                            )
                        )
                    if actual_sha != expected_sha:
                        differences.append(
                            self._difference_entry(
                                snapshot,
                                domain,
                                "digest_mismatch",
                                expected_length,
                                expected_sha,
                                actual_length,
                                actual_sha,
                            )
                        )

            snapshot_ids = [str(item["version_id"]) for item in snapshots]
            final_statuses: dict[str, str] = {}
            if snapshot_ids:
                placeholders = ",".join("?" for _ in snapshot_ids)
                with self.database.transaction() as connection:
                    for row in connection.execute(
                        f"SELECT version_id, status FROM object_versions "
                        f"WHERE version_id IN ({placeholders})",
                        snapshot_ids,
                    ).fetchall():
                        final_statuses[str(row["version_id"])] = str(row["status"])
            eligible_ids = {
                version_id
                for version_id, status in final_statuses.items()
                if status == "complete"
            }
            differences = [
                item
                for item in differences
                if item["version_id"] in eligible_ids
            ]
            kind_order = {"missing": 0, "length_mismatch": 1, "digest_mismatch": 2}
            differences.sort(
                key=lambda item: (
                    item["object_key"],
                    item["version_number"],
                    item["version_id"],
                    item["domain"],
                    kind_order[item["kind"]],
                )
            )
            missing = [item for item in differences if item["kind"] == "missing"]
            length_mismatches = [
                item for item in differences if item["kind"] == "length_mismatch"
            ]
            digest_mismatches = [
                item for item in differences if item["kind"] == "digest_mismatch"
            ]
            finished = utc_now()
            summary = {
                "checked_domain_copies": checked,
                "difference_count": len(differences),
                "differences": differences,
                "digest_mismatches": digest_mismatches,
                "finished_at": format_time(finished),
                "length_mismatches": length_mismatches,
                "missing_domains": missing,
                "reconciliation_id": reconciliation_id,
                "snapshot_version_count": len(snapshots),
                "started_at": format_time(started),
                "status": "completed",
            }
            self._state_set("last_reconciliation", summary)
            return summary
        finally:
            self._reconcile_mutex.release()

    @staticmethod
    def _difference_entry(
        snapshot: dict[str, Any],
        domain: str,
        kind: str,
        expected_length: int,
        expected_sha: str,
        actual_length: int | None,
        actual_sha: str | None,
    ) -> dict[str, Any]:
        return {
            "actual_length": actual_length,
            "actual_sha256": actual_sha,
            "domain": domain,
            "expected_length": expected_length,
            "expected_sha256": expected_sha,
            "kind": kind,
            "object_key": snapshot["object_key"],
            "version_id": snapshot["version_id"],
            "version_number": snapshot["version_number"],
        }

    # ------------------------------------------------------------------
    # Policy impact report
    # ------------------------------------------------------------------

    def policy_impact(
        self,
        *,
        regular_days: int,
        audit_days: int,
        legal_days: int,
        effective_at: str | datetime | None = None,
        as_of: str | datetime | None = None,
    ) -> dict[str, Any]:
        proposed = {
            "regular": self._validate_days("regular", regular_days),
            "audit": self._validate_days("audit", audit_days),
            "legal": self._validate_days("legal", legal_days),
        }
        generated = utc_now()
        effective = self._parse_time_argument(
            effective_at, field="effective_at", default=generated
        )
        evaluation = self._parse_time_argument(
            as_of, field="as_of", default=generated
        )
        with self.database.transaction(immediate=True) as connection:
            self._activate_due_policies(connection, generated)
            baseline_row = self._policy_at(connection, generated)
            if baseline_row is None:
                baseline_row = self._latest_policy(connection)
            if baseline_row is None:
                raise ApiError(
                    "POLICY_NOT_INITIALIZED", 500, "no retention policy exists"
                )
            baseline = self._policy_values(
                connection, int(baseline_row["publication_id"])
            )
            shortened_categories = {
                category: {
                    "current": baseline[category],
                    "proposed": proposed[category],
                }
                for category in CATEGORIES
                if proposed[category] < baseline[category]
            }
            rejected = bool(shortened_categories)
            rows = connection.execute(
                "SELECT v.*, l.retention_until AS lock_retention_until, "
                "l.lock_pinned AS lock_pinned "
                "FROM object_versions v LEFT JOIN lock_records l "
                "ON l.version_id = v.version_id "
                "WHERE v.status != 'deleted' "
                "ORDER BY v.object_key ASC, v.version_number ASC, v.version_id ASC"
            ).fetchall()
            affected: list[dict[str, Any]] = []
            admission_changes: list[dict[str, Any]] = []
            deadline_changed = 0
            would_shorten = 0
            would_shorten_keys: set[str] = set()
            for row in rows:
                category = str(row["category"])
                legal_hold = bool(row["legal_hold"])
                written_at = parse_time(row["written_at"])
                baseline_deadline = add_days(written_at, baseline[category])
                candidate = add_days(written_at, proposed[category])
                if legal_hold:
                    baseline_deadline = max(
                        baseline_deadline, add_days(written_at, baseline["legal"])
                    )
                    candidate = max(candidate, add_days(written_at, proposed["legal"]))
                baseline_text = format_time(baseline_deadline)
                candidate_text = format_time(candidate)
                current_meta = str(row["retention_until"])
                current_lock = (
                    str(row["lock_retention_until"])
                    if row["lock_retention_until"] is not None
                    else None
                )
                current_effective = (
                    max(current_meta, current_lock)
                    if current_lock is not None
                    else current_meta
                )
                if rejected and candidate_text < current_effective:
                    would_shorten += 1
                    would_shorten_keys.add(str(row["object_key"]))

                if rejected:
                    metadata_after = current_meta
                    lock_after = current_lock
                else:
                    metadata_after = max(current_meta, candidate_text)
                    if current_lock is None:
                        lock_after = None
                    elif int(row["lock_pinned"] or 0):
                        lock_after = current_lock
                    else:
                        lock_after = max(current_lock, candidate_text)
                changed = (
                    metadata_after != current_meta or lock_after != current_lock
                )
                if changed:
                    deadline_changed += 1
                category_changed = baseline[category] != proposed[category] or (
                    legal_hold and baseline["legal"] != proposed["legal"]
                )
                if category_changed or changed:
                    affected.append(
                        {
                            "baseline_retention_until": baseline_text,
                            "candidate_retention_until": candidate_text,
                            "category": category,
                            "current_effective_retention_until": current_effective,
                            "current_lock_retention_until": current_lock,
                            "current_metadata_retention_until": current_meta,
                            "deadline_changed": changed,
                            "legal_hold": legal_hold,
                            "object_key": row["object_key"],
                            "proposed_effective_retention_until": (
                                max(metadata_after, lock_after)
                                if lock_after is not None
                                else metadata_after
                            ),
                            "version_id": row["version_id"],
                            "version_number": int(row["version_number"]),
                        }
                    )

                before = self._admission_values(
                    version_id=str(row["version_id"]),
                    object_key=str(row["object_key"]),
                    version_number=int(row["version_number"]),
                    status=str(row["status"]),
                    legal_hold=legal_hold,
                    metadata_retention_until=current_meta,
                    lock_retention_until=current_lock,
                    now=evaluation,
                )
                after = self._admission_values(
                    version_id=str(row["version_id"]),
                    object_key=str(row["object_key"]),
                    version_number=int(row["version_number"]),
                    status=str(row["status"]),
                    legal_hold=legal_hold,
                    metadata_retention_until=metadata_after,
                    lock_retention_until=lock_after,
                    now=evaluation,
                )
                if (
                    before["allowed"] != after["allowed"]
                    or before["reason_code"] != after["reason_code"]
                ):
                    admission_changes.append(
                        {
                            "after": {
                                "allowed": after["allowed"],
                                "reason_code": after["reason_code"],
                                "reason_codes": after["reason_codes"],
                                "resolved_retention_until": after[
                                    "retention_resolution"
                                ]["effective_retention_until"],
                            },
                            "before": {
                                "allowed": before["allowed"],
                                "reason_code": before["reason_code"],
                                "reason_codes": before["reason_codes"],
                                "resolved_retention_until": before[
                                    "retention_resolution"
                                ]["effective_retention_until"],
                            },
                            "change": self._admission_change_name(before, after),
                            "object_key": row["object_key"],
                            "version_id": row["version_id"],
                            "version_number": int(row["version_number"]),
                        }
                    )

            canonical = {
                "baseline_publication_id": int(baseline_row["publication_id"]),
                "proposed": proposed,
                "version_ids": sorted(str(row["version_id"]) for row in rows),
            }
            report_id = hashlib.sha256(_json_dump(canonical).encode("utf-8")).hexdigest()
            report = {
                "affected_version_count": len(affected),
                "affected_versions": affected,
                "affected_object_count": len(
                    {item["object_key"] for item in affected}
                ),
                "as_of": format_time(evaluation),
                "baseline_publication_id": int(baseline_row["publication_id"]),
                "baseline_retention_days": baseline,
                "cleanup_admission_changed_count": len(admission_changes),
                "cleanup_admission_changes": admission_changes,
                "effective_at": format_time(effective),
                "generated_at": format_time(generated),
                "object_count": len({str(row["object_key"]) for row in rows}),
                "proposed_retention_days": proposed,
                "publication_status": "rejected" if rejected else "acceptable",
                "rejection_code": (
                    "RETENTION_POLICY_SHORTENING_REJECTED" if rejected else None
                ),
                "report_id": report_id,
                "retention_deadline_changed_count": deadline_changed,
                "retention_deadline_changed_object_count": len(
                    {
                        item["object_key"]
                        for item in affected
                        if item["deadline_changed"]
                    }
                ),
                "shortened_categories": shortened_categories,
                "total_version_count": len(rows),
                "would_shorten_rejected_count": would_shorten,
                "would_shorten_rejected_object_count": len(would_shorten_keys),
            }
            return report

    def policy_impact_for_publication(
        self,
        publication_id: int,
        *,
        as_of: str | datetime | None = None,
    ) -> dict[str, Any]:
        generated = utc_now()
        evaluation = self._parse_time_argument(
            as_of, field="as_of", default=generated
        )
        with self.database.transaction() as connection:
            publication = connection.execute(
                "SELECT * FROM policy_publications WHERE publication_id = ?",
                (int(publication_id),),
            ).fetchone()
            if publication is None:
                raise ApiError(
                    "POLICY_PUBLICATION_NOT_FOUND",
                    404,
                    "policy publication does not exist",
                    {"publication_id": publication_id},
                )
            current_values = self._policy_values(
                connection, int(publication["publication_id"])
            )
            baseline_row = connection.execute(
                "SELECT * FROM policy_publications "
                "WHERE effective_at < ? "
                "OR (effective_at = ? AND publication_id < ?) "
                "ORDER BY effective_at DESC, publication_id DESC LIMIT 1",
                (
                    publication["effective_at"],
                    publication["effective_at"],
                    publication["publication_id"],
                ),
            ).fetchone()
            baseline_values = (
                self._policy_values(
                    connection, int(baseline_row["publication_id"])
                )
                if baseline_row is not None
                else current_values
            )
            rows = connection.execute(
                "SELECT v.*, l.retention_until AS lock_retention_until, "
                "l.lock_pinned AS lock_pinned "
                "FROM object_versions v LEFT JOIN lock_records l "
                "ON l.version_id = v.version_id "
                "WHERE v.created_at <= ? AND v.status != 'deleted' "
                "ORDER BY v.object_key ASC, v.version_number ASC, v.version_id ASC",
                (publication["created_at"],),
            ).fetchall()
            events = {
                str(row["version_id"]): row
                for row in connection.execute(
                    "SELECT * FROM policy_extension_events WHERE publication_id = ?",
                    (publication["publication_id"],),
                ).fetchall()
            }
            affected: list[dict[str, Any]] = []
            changes: list[dict[str, Any]] = []
            deadline_changed = 0
            shortening_prevented = 0
            shortening_prevented_keys: set[str] = set()
            for row in rows:
                version_id = str(row["version_id"])
                event = events.get(version_id)
                current_metadata = str(row["retention_until"])
                current_lock = (
                    str(row["lock_retention_until"])
                    if row["lock_retention_until"] is not None
                    else None
                )
                old_metadata = (
                    str(event["old_metadata_until"])
                    if event is not None
                    else current_metadata
                )
                new_metadata = (
                    str(event["new_metadata_until"])
                    if event is not None
                    else current_metadata
                )
                old_lock = (
                    event["old_lock_until"] if event is not None else current_lock
                )
                new_lock = (
                    event["new_lock_until"] if event is not None else current_lock
                )
                category = str(row["category"])
                legal_hold = bool(row["legal_hold"])
                written_at = parse_time(row["written_at"])
                candidate = add_days(written_at, current_values[category])
                if legal_hold:
                    candidate = max(
                        candidate, add_days(written_at, current_values["legal"])
                    )
                candidate_text = format_time(candidate)
                old_effective = (
                    max(old_metadata, old_lock)
                    if old_lock is not None
                    else old_metadata
                )
                if candidate_text < old_effective:
                    shortening_prevented += 1
                    shortening_prevented_keys.add(str(row["object_key"]))
                changed = event is not None
                category_changed = (
                    baseline_values[category] != current_values[category]
                    or (
                        legal_hold
                        and baseline_values["legal"] != current_values["legal"]
                    )
                )
                if changed:
                    deadline_changed += 1
                if changed or category_changed:
                    effective_after = (
                        max(new_metadata, new_lock)
                        if new_lock is not None
                        else new_metadata
                    )
                    affected.append(
                        {
                            "candidate_retention_until": candidate_text,
                            "category": category,
                            "current_effective_retention_until": effective_after,
                            "current_lock_retention_until": new_lock,
                            "current_metadata_retention_until": new_metadata,
                            "deadline_changed": changed,
                            "legal_hold": legal_hold,
                            "object_key": row["object_key"],
                            "previous_effective_retention_until": old_effective,
                            "previous_lock_retention_until": old_lock,
                            "previous_metadata_retention_until": old_metadata,
                            "proposed_effective_retention_until": effective_after,
                            "version_id": version_id,
                            "version_number": int(row["version_number"]),
                        }
                    )

                before = self._admission_values(
                    version_id=version_id,
                    object_key=str(row["object_key"]),
                    version_number=int(row["version_number"]),
                    status=str(row["status"]),
                    legal_hold=legal_hold,
                    metadata_retention_until=old_metadata,
                    lock_retention_until=old_lock,
                    now=evaluation,
                )
                after = self._admission_values(
                    version_id=version_id,
                    object_key=str(row["object_key"]),
                    version_number=int(row["version_number"]),
                    status=str(row["status"]),
                    legal_hold=legal_hold,
                    metadata_retention_until=new_metadata,
                    lock_retention_until=new_lock,
                    now=evaluation,
                )
                if (
                    before["allowed"] != after["allowed"]
                    or before["reason_code"] != after["reason_code"]
                ):
                    changes.append(
                        {
                            "after": {
                                "allowed": after["allowed"],
                                "reason_code": after["reason_code"],
                                "reason_codes": after["reason_codes"],
                                "resolved_retention_until": after[
                                    "retention_resolution"
                                ]["effective_retention_until"],
                            },
                            "before": {
                                "allowed": before["allowed"],
                                "reason_code": before["reason_code"],
                                "reason_codes": before["reason_codes"],
                                "resolved_retention_until": before[
                                    "retention_resolution"
                                ]["effective_retention_until"],
                            },
                            "change": self._admission_change_name(before, after),
                            "object_key": row["object_key"],
                            "version_id": version_id,
                            "version_number": int(row["version_number"]),
                        }
                    )
            canonical = {
                "publication_id": int(publication["publication_id"]),
                "version_ids": sorted(str(row["version_id"]) for row in rows),
            }
            return {
                "affected_version_count": len(affected),
                "affected_versions": affected,
                "affected_object_count": len(
                    {item["object_key"] for item in affected}
                ),
                "as_of": format_time(evaluation),
                "baseline_publication_id": (
                    int(baseline_row["publication_id"])
                    if baseline_row is not None
                    else None
                ),
                "baseline_retention_days": baseline_values,
                "cleanup_admission_changed_count": len(changes),
                "cleanup_admission_changes": changes,
                "effective_at": publication["effective_at"],
                "generated_at": format_time(generated),
                "object_count": len({str(row["object_key"]) for row in rows}),
                "publication_id": int(publication["publication_id"]),
                "publication_status": "published",
                "published_retention_days": current_values,
                "report_id": hashlib.sha256(
                    _json_dump(canonical).encode("utf-8")
                ).hexdigest(),
                "retention_deadline_changed_count": deadline_changed,
                "retention_deadline_changed_object_count": len(
                    {
                        item["object_key"]
                        for item in affected
                        if item["deadline_changed"]
                    }
                ),
                "shortening_prevented_count": shortening_prevented,
                "shortening_prevented_object_count": len(
                    shortening_prevented_keys
                ),
                "total_version_count": len(rows),
                "would_shorten_rejected_count": 0,
            }

    @staticmethod
    def _admission_change_name(before: dict[str, Any], after: dict[str, Any]) -> str:
        if before["allowed"] and not after["allowed"]:
            return "became_denied"
        if not before["allowed"] and after["allowed"]:
            return "became_eligible"
        return "reason_changed"

    # ------------------------------------------------------------------
    # Test/operations hooks
    # ------------------------------------------------------------------

    def override_retention(
        self,
        version_id: str,
        *,
        metadata_retention_until: str | datetime | None = None,
        lock_retention_until: str | datetime | None = None,
        legal_hold: bool | None = None,
        lock_pinned: bool | None = None,
        written_at: str | datetime | None = None,
    ) -> dict[str, Any]:
        with self.database.transaction(immediate=True) as connection:
            row = self._version_row(connection, version_id)
            updates: list[str] = []
            parameters: list[Any] = []
            if metadata_retention_until is not None:
                updates.append("retention_until = ?")
                parameters.append(
                    format_time(
                        self._parse_time_argument(
                            metadata_retention_until,
                            field="metadata_retention_until",
                        )
                    )
                )
            if legal_hold is not None:
                updates.append("legal_hold = ?")
                parameters.append(1 if legal_hold else 0)
            if written_at is not None:
                updates.append("written_at = ?")
                parameters.append(
                    format_time(
                        self._parse_time_argument(
                            written_at, field="written_at"
                        )
                    )
                )
            if updates:
                parameters.append(version_id)
                connection.execute(
                    f"UPDATE object_versions SET {', '.join(updates)} "
                    "WHERE version_id = ?",
                    parameters,
                )
            lock = connection.execute(
                "SELECT * FROM lock_records WHERE version_id = ?",
                (version_id,),
            ).fetchone()
            if lock_retention_until is not None or lock_pinned is not None:
                lock_time = (
                    format_time(
                        self._parse_time_argument(
                            lock_retention_until,
                            field="lock_retention_until",
                        )
                    )
                    if lock_retention_until is not None
                    else (lock["retention_until"] if lock is not None else row["retention_until"])
                )
                pinned = (
                    1 if lock_pinned else 0
                    if lock_pinned is not None
                    else int(lock["lock_pinned"] if lock is not None else 0)
                )
                connection.execute(
                    "INSERT INTO lock_records("
                    "version_id, retention_until, lock_pinned, updated_at"
                    ") VALUES (?, ?, ?, ?) "
                    "ON CONFLICT(version_id) DO UPDATE SET "
                    "retention_until = excluded.retention_until, "
                    "lock_pinned = excluded.lock_pinned, updated_at = excluded.updated_at",
                    (version_id, lock_time, pinned, format_time(utc_now())),
                )
        return self.version_metadata(version_id)

    def create_domain_difference(self, version_id: str, *, domain: str, mode: str) -> dict[str, Any]:
        if domain not in DOMAINS:
            raise ApiError(
                "UNKNOWN_STORAGE_DOMAIN",
                400,
                "unknown storage domain",
                {"allowed": list(DOMAINS), "received": domain},
            )
        path = self._domain_path(domain, version_id)
        if mode == "missing":
            remove_quietly(path)
        elif mode == "truncate":
            try:
                size = path.stat().st_size
                with open(path, "r+b") as handle:
                    handle.truncate(max(0, size - max(1, size // 7)))
            except FileNotFoundError:
                raise ApiError(
                    "DOMAIN_COPY_MISSING",
                    404,
                    "domain copy is already missing",
                    {"domain": domain, "version_id": version_id},
                )
        elif mode == "corrupt":
            try:
                with open(path, "r+b") as handle:
                    data = handle.read(4096)
                    if not data:
                        handle.write(b"X")
                    else:
                        offset = len(data) // 2
                        handle.seek(offset)
                        original = data[offset]
                        handle.write(bytes([original ^ 0xFF]))
            except FileNotFoundError:
                raise ApiError(
                    "DOMAIN_COPY_MISSING",
                    404,
                    "domain copy is missing",
                    {"domain": domain, "version_id": version_id},
                )
        else:
            raise ApiError(
                "INVALID_DIFFERENCE_MODE",
                400,
                "mode must be missing, truncate or corrupt",
                {"received": mode},
            )
        return {
            "domain": domain,
            "mode": mode,
            "version_id": version_id,
        }

    # ------------------------------------------------------------------
    # State and observability
    # ------------------------------------------------------------------

    def _state_set(self, key: str, value: Any) -> None:
        with self.database.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO service_state(key, value, updated_at) VALUES (?, ?, ?) "
                "ON CONFLICT(key) DO UPDATE SET "
                "value = excluded.value, updated_at = excluded.updated_at",
                (key, _json_dump(value), format_time(utc_now())),
            )

    def _pending_cleanup_count(self, connection: sqlite3.Connection) -> int:
        candidates, _ = self._candidate_versions(connection, utc_now())
        return len(candidates)

    def status(self) -> dict[str, Any]:
        now = utc_now()
        with self.database.transaction(immediate=True) as connection:
            self._activate_due_policies(connection, now)
            active = self._policy_at(connection, now)
            pending = self._pending_cleanup_count(connection)
            version_count = int(
                connection.execute(
                    "SELECT COUNT(*) FROM object_versions WHERE status != 'deleted'"
                ).fetchone()[0]
            )
            deleted_count = int(
                connection.execute(
                    "SELECT COUNT(*) FROM object_versions WHERE status = 'deleted'"
                ).fetchone()[0]
            )
            policy = (
                self._policy_publication_json(connection, active)
                if active is not None
                else None
            )
            state_rows = {
                str(row["key"]): row["value"]
                for row in connection.execute(
                    "SELECT key, value FROM service_state "
                    "WHERE key IN ('last_cleanup', 'last_reconciliation')"
                ).fetchall()
            }
            last_cleanup = _json_load(state_rows.get("last_cleanup"), None)
            last_reconciliation = _json_load(
                state_rows.get("last_reconciliation"), None
            )
        return {
            "active_policy": policy,
            "deleted_version_count": deleted_count,
            "generated_at": format_time(now),
            "last_cleanup": last_cleanup,
            "last_reconciliation": last_reconciliation,
            "pending_cleanup_version_count": pending,
            "service": "compliant-archive",
            "status": "ok",
            "version_count": version_count,
        }

    def metrics_text(self) -> str:
        now = utc_now()
        with self.database.transaction(immediate=True) as connection:
            self._activate_due_policies(connection, now)
            domain_rows = connection.execute(
                "SELECT d.domain, COUNT(*) AS versions, "
                "COALESCE(SUM(d.expected_length), 0) AS bytes "
                "FROM domain_copies d JOIN object_versions v "
                "ON v.version_id = d.version_id "
                "WHERE d.write_status = 'written' "
                "AND d.delete_status IN ('present', 'failed') "
                "AND v.status != 'deleted' "
                "GROUP BY d.domain"
            ).fetchall()
            domain_values = {
                str(row["domain"]): (int(row["versions"]), int(row["bytes"]))
                for row in domain_rows
            }
            pending = self._pending_cleanup_count(connection)
            denials_row = connection.execute(
                "SELECT value FROM counters "
                "WHERE name = 'retention_conflict_delete_denials'"
            ).fetchone()
            denials = int(denials_row["value"]) if denials_row is not None else 0
            state_row = connection.execute(
                "SELECT value FROM service_state WHERE key = 'last_reconciliation'"
            ).fetchone()
            last_reconcile = _json_load(
                state_row["value"] if state_row is not None else None, None
            )
            diff_count = (
                int(last_reconcile.get("difference_count", 0))
                if isinstance(last_reconcile, dict)
                else 0
            )
        lines = [
            "# HELP archive_domain_object_versions Current version copies stored in each storage domain.",
            "# TYPE archive_domain_object_versions gauge",
        ]
        for domain in DOMAINS:
            versions, _ = domain_values.get(domain, (0, 0))
            lines.append(
                f'archive_domain_object_versions{{domain="{domain}"}} {versions}'
            )
        lines.extend(
            [
                "# HELP archive_domain_bytes Current logical bytes stored in each storage domain.",
                "# TYPE archive_domain_bytes gauge",
            ]
        )
        for domain in DOMAINS:
            _, byte_count = domain_values.get(domain, (0, 0))
            lines.append(f'archive_domain_bytes{{domain="{domain}"}} {byte_count}')
        lines.extend(
            [
                "# HELP archive_pending_cleanup_versions Expired versions still awaiting completed cleanup.",
                "# TYPE archive_pending_cleanup_versions gauge",
                f"archive_pending_cleanup_versions {pending}",
                "# HELP archive_retention_conflict_delete_denials_total Delete attempts denied because conflicting deadlines resolve to a future time.",
                "# TYPE archive_retention_conflict_delete_denials_total counter",
                f"archive_retention_conflict_delete_denials_total {denials}",
                "# HELP archive_reconciliation_differences Differences reported by the most recent reconciliation.",
                "# TYPE archive_reconciliation_differences gauge",
                f"archive_reconciliation_differences {diff_count}",
                "",
            ]
        )
        return "\n".join(lines)
