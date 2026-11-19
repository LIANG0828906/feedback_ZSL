from __future__ import annotations

import hashlib
import json
import os
import shutil
import threading
from pathlib import Path
from typing import Any

from .auditor import audit_repository
from .common import (
    BC_DRILL_BLOCK_DIGEST_MISMATCH,
    BC_DRILL_BLOCK_MISSING,
    BC_DRILL_BLOCK_READ_ERROR,
    BC_DRILL_OUTPUT_INCOMPLETE,
    BC_DRILL_STAGING_MISSING,
    BC_RESTORE_POINT_UNAVAILABLE,
    BusinessError,
    ProcessLock,
    SCHEMA_VERSION,
    SimulatedInterrupt,
    canonical_json,
    safe_slug,
    sha256_bytes,
    utc_now,
    validate_separate_paths,
)
from .store import Store


AUDIT_CATALOG_KEYS = {
    "block_count",
    "breakpoint",
    "chain_id",
    "conflict_blocks",
    "half_written_blocks",
    "increment_count",
    "last_usable_sequence",
    "manifest_path",
    "manifest_sha256",
    "missing_blocks",
    "status",
    "undeclared_copies",
    "unconfirmed_blocks",
    "usable_restore_points",
}


class BackupChainService:
    def __init__(
        self,
        *,
        repo: Path,
        manifest: Path,
        database: Path,
        output_dir: Path,
    ) -> None:
        self.repo = repo.resolve()
        self.manifest = manifest.resolve()
        self.database = database.resolve()
        self.output_dir = output_dir.resolve()
        validate_separate_paths(self.repo, self.database, self.output_dir)
        if not self.repo.is_dir():
            raise BusinessError(
                "BC_REPOSITORY_NOT_FOUND",
                "backup repository directory does not exist",
                repository=str(self.repo),
            )
        if not self.manifest.is_file():
            raise BusinessError(
                "BC_MANIFEST_NOT_FOUND",
                "backup manifest file does not exist",
                manifest=str(self.manifest),
            )
        self.store = Store(self.database)
        self._audit_lock = threading.Lock()
        self._drill_lock = threading.Lock()
        self.output_dir.mkdir(parents=True, exist_ok=True)
        self._audit_file_lock = ProcessLock(
            self.database.with_name(self.database.name + ".audit.lock"),
            operation="chain audit",
        )
        self._drill_file_lock = ProcessLock(
            self.output_dir / ".locks" / "drill.lock",
            operation="restore drill",
        )

    def close(self) -> None:
        self.store.close()

    def audit(self) -> dict[str, Any]:
        if not self._audit_lock.acquire(blocking=False):
            raise BusinessError(
                "BC_AUDIT_IN_PROGRESS",
                "another chain audit is already running in this service",
            )
        try:
            self._audit_file_lock.acquire()
            try:
                outcome = audit_repository(self.repo, self.manifest)
                committed = self.store.commit_audit(outcome, self.repo)
                return {
                    "audit_id": committed["audit_id"],
                    "catalog": outcome.catalog,
                    "errors": outcome.errors,
                    "injection_impact": outcome.injection_impact,
                    "restore_points": outcome.restore_points,
                    "schema_version": SCHEMA_VERSION,
                    "unusable_blocks": outcome.unusable_blocks,
                    "view_version": committed["view_version"],
                }
            finally:
                self._audit_file_lock.release()
        finally:
            self._audit_lock.release()

    def _require_audit(
        self, *, chain_id: str | None, audit_id: int | None
    ) -> dict[str, Any]:
        audit = self.store.get_audit(audit_id=audit_id, chain_id=chain_id)
        if audit is None:
            raise BusinessError(
                "BC_AUDIT_NOT_FOUND",
                "no committed audit matches the requested chain",
                chain_id=chain_id,
                audit_id=audit_id,
            )
        if chain_id is not None and audit["chain_id"] != chain_id:
            raise BusinessError(
                "BC_CHAIN_AUDIT_MISMATCH",
                "the requested audit belongs to another chain",
                chain_id=chain_id,
                audit_id=audit["audit_id"],
            )
        return audit

    @staticmethod
    def _drill_public(drill: dict[str, Any] | None) -> dict[str, Any] | None:
        if drill is None:
            return None
        block_count = int(drill["block_count"])
        progress_blocks = int(drill["progress_blocks"])
        ratio = (
            round(progress_blocks / block_count, 6) if block_count else 1.0
        )
        return {
            "audit_id": drill["audit_id"],
            "block_count": block_count,
            "blocks_read": drill["blocks_read"],
            "blocks_skipped": drill["blocks_skipped"],
            "blocks_written": drill["blocks_written"],
            "bytes_read": drill["bytes_read"],
            "bytes_written": drill["bytes_written"],
            "chain_id": drill["chain_id"],
            "completed_at": drill["completed_at"],
            "conclusion": drill["conclusion"],
            "drill_id": drill["drill_id"],
            "failure": drill["failure"],
            "last_progress": drill["last_progress"],
            "output_path": drill["output_path"],
            "progress": {
                "blocks": progress_blocks,
                "bytes": int(drill["progress_bytes"]),
                "ratio": ratio,
            },
            "resume_count": drill["resume_count"],
            "sequence": drill["sequence"],
            "state_digest": drill["state_digest"],
            "status": drill["status"],
            "updated_at": drill["updated_at"],
        }

    def status(
        self,
        *,
        chain_id: str | None,
        restore_point: int | None,
        audit_id: int | None,
    ) -> dict[str, Any]:
        audit = self._require_audit(chain_id=chain_id, audit_id=audit_id)
        explicit_point = restore_point is not None
        if restore_point is None:
            restore_point = audit["last_usable_sequence"]
        if restore_point is None:
            raise BusinessError(
                BC_RESTORE_POINT_UNAVAILABLE,
                "the audit has no usable restore point",
                chain_id=audit["chain_id"],
                audit_id=audit["audit_id"],
            )
        requested_sequence = int(restore_point)
        point = self.store.restore_point(audit["audit_id"], requested_sequence)
        if point is None or not point["usable"]:
            if not explicit_point:
                raise BusinessError(
                    BC_RESTORE_POINT_UNAVAILABLE,
                    "the audit has no usable restore point",
                    chain_id=audit["chain_id"],
                    audit_id=audit["audit_id"],
                )
            point = {
                "audit_id": audit["audit_id"],
                "backup_id": None,
                "block_count": 0,
                "kind": "unavailable",
                "sequence": requested_sequence,
                "state_digest": None,
                "usable": False,
            }
            blocks = []
            drill = None
        else:
            blocks = self.store.restore_blocks(
                audit["audit_id"], requested_sequence
            )
            drill = self.store.latest_drill_for_point(
                audit["audit_id"], requested_sequence
            )
        injection = self.store.latest_injection_impact(audit["audit_id"])
        return {
            "audit": {
                "audit_id": audit["audit_id"],
                "chain_id": audit["chain_id"],
                "created_at": audit["created_at"],
                "manifest_sha256": audit["manifest_sha256"],
                "status": audit["status"],
                "view_version": audit["view_version"],
            },
            "breakpoint": audit["breakpoint"],
            "chain_id": audit["chain_id"],
            "drill": self._drill_public(drill),
            "recent_injection_impact": injection,
            "restore_point": point,
            "schema_version": SCHEMA_VERSION,
            "selected_blocks": sorted(
                blocks, key=lambda item: (item["ordinal"], item["block_id"])
            ),
        }

    @staticmethod
    def _selection_digest(blocks: list[dict[str, Any]]) -> str:
        normalized = [
            {
                "block_id": block["block_id"],
                "length": block["length"],
                "ordinal": block["ordinal"],
                "sha256": block["sha256"],
            }
            for block in sorted(
                blocks, key=lambda item: (item["ordinal"], item["block_id"])
            )
        ]
        return sha256_bytes(canonical_json(normalized).encode("utf-8"))

    def _drill_id(
        self, *, chain_id: str, audit_id: int, sequence: int, state_digest: str,
        selection_digest: str
    ) -> str:
        payload = (
            f"{chain_id}|{audit_id}|{sequence}|{state_digest}|{selection_digest}"
        )
        return sha256_bytes(payload.encode("utf-8"))

    def _output_path(
        self, *, chain_id: str, audit_id: int, sequence: int
    ) -> Path:
        return self.output_dir / (
            f"restore-{safe_slug(chain_id)}-seq-{sequence:03d}-audit-{audit_id}.bin"
        )

    def _fail_preflight(
        self,
        *,
        drill_id: str,
        block: dict[str, Any],
        code: str,
        detail: Any,
    ) -> dict[str, Any]:
        failed = self.store.fail_drill_block(
            drill_id=drill_id,
            block=block,
            code=code,
            actual_sha256=None,
            detail=detail,
            read_bytes=0,
            write_bytes=0,
        )
        return self._drill_public(failed) or {}

    def run_drill(
        self, *, restore_point: int, audit_id: int | None = None
    ) -> dict[str, Any]:
        if not self._drill_lock.acquire(blocking=False):
            raise BusinessError(
                "BC_DRILL_IN_PROGRESS",
                "another restore drill is already running in this service",
            )
        try:
            self._drill_file_lock.acquire()
            try:
                return self._run_drill_locked(
                    restore_point=restore_point, audit_id=audit_id
                )
            finally:
                self._drill_file_lock.release()
        finally:
            self._drill_lock.release()

    def _run_drill_locked(
        self, *, restore_point: int, audit_id: int | None
    ) -> dict[str, Any]:
            audit = self._require_audit(chain_id=None, audit_id=audit_id)
            point = self.store.restore_point(audit["audit_id"], int(restore_point))
            if point is None or not point["usable"]:
                raise BusinessError(
                    BC_RESTORE_POINT_UNAVAILABLE,
                    "the requested restore point is not usable",
                    chain_id=audit["chain_id"],
                    audit_id=audit["audit_id"],
                    restore_point=int(restore_point),
                )
            blocks = self.store.restore_blocks(
                audit["audit_id"], int(restore_point)
            )
            if (
                len(blocks) != point["block_count"]
                or not blocks
                or len({block["block_id"] for block in blocks}) != len(blocks)
            ):
                raise BusinessError(
                    "BC_DRILL_SELECTION_INCOMPLETE",
                    "committed restore-point block selection is incomplete",
                    audit_id=audit["audit_id"],
                    restore_point=int(restore_point),
                )
            ordinal_values = [block["ordinal"] for block in blocks]
            if len(ordinal_values) != len(set(ordinal_values)):
                raise BusinessError(
                    "BC_DRILL_SELECTION_INVALID",
                    "restore-point block ordinals are not unique",
                    audit_id=audit["audit_id"],
                    restore_point=int(restore_point),
                )

            selection_digest = self._selection_digest(blocks)
            drill_id = self._drill_id(
                chain_id=audit["chain_id"],
                audit_id=audit["audit_id"],
                sequence=int(restore_point),
                state_digest=point["state_digest"],
                selection_digest=selection_digest,
            )
            final_path = self._output_path(
                chain_id=audit["chain_id"],
                audit_id=audit["audit_id"],
                sequence=int(restore_point),
            )
            staging_dir = self.output_dir / ".staging"
            staging_dir.mkdir(parents=True, exist_ok=True)
            staging_path = staging_dir / f"{drill_id}.part"
            drill, committed_raw, idempotent = self.store.begin_drill(
                drill_id=drill_id,
                chain_id=audit["chain_id"],
                audit_id=audit["audit_id"],
                sequence=int(restore_point),
                state_digest=point["state_digest"],
                selection_digest=selection_digest,
                block_count=len(blocks),
                staging_path=str(staging_path),
            )
            if idempotent or drill["status"] == "completed":
                try:
                    staging_path.unlink()
                except FileNotFoundError:
                    pass
                return {
                    **self._drill_public(drill),
                    "idempotent_replay": True,
                }
            if final_path.exists():
                # A previous hard crash may have published the file before the
                # completion transaction.  Output is authoritative only when
                # the SQLite conclusion is committed.
                try:
                    final_path.unlink()
                except OSError:
                    pass

            committed = [
                {
                    "actual_sha256": item["actual_sha256"],
                    "block_id": item["block_id"],
                    "length": item["length"],
                    "ordinal": item["ordinal"],
                    "path": item["path"],
                    "sha256": item.get("sha256")
                    or item.get("expected_sha256"),
                }
                for item in self.store.committed_drill_blocks(drill_id)
            ]
            expected_prefix = [block["block_id"] for block in blocks]
            committed_ids = [item["block_id"] for item in committed]
            if committed_ids != expected_prefix[: len(committed_ids)]:
                first_uncommitted = blocks[len(committed_ids)]
                return self._fail_preflight(
                    drill_id=drill_id,
                    block=first_uncommitted,
                    code=BC_DRILL_STAGING_MISSING,
                    detail={
                        "message": "committed progress is not a prefix of the frozen selection",
                        "committed_block_ids": committed_ids,
                    },
                )
            progress_bytes = sum(item["length"] for item in committed)

            if drill["progress_blocks"] != len(committed):
                first_uncommitted = blocks[min(len(committed), len(blocks) - 1)]
                return self._fail_preflight(
                    drill_id=drill_id,
                    block=first_uncommitted,
                    code=BC_DRILL_STAGING_MISSING,
                    detail={
                        "message": "progress counter does not match committed blocks",
                        "progress_blocks": drill["progress_blocks"],
                        "committed_blocks": len(committed),
                    },
                )
            if len(committed) < len(blocks):
                if not staging_path.exists():
                    if committed:
                        return self._fail_preflight(
                            drill_id=drill_id,
                            block=blocks[len(committed)],
                            code=BC_DRILL_STAGING_MISSING,
                            detail={
                                "message": "staging file needed for resume is missing",
                                "staging_path": str(staging_path),
                            },
                        )
                    staging_path.touch()
                elif staging_path.stat().st_size < progress_bytes:
                    return self._fail_preflight(
                        drill_id=drill_id,
                        block=blocks[len(committed)],
                        code=BC_DRILL_STAGING_MISSING,
                        detail={
                            "message": "staging file is shorter than committed progress",
                            "expected_at_least": progress_bytes,
                            "actual": staging_path.stat().st_size,
                        },
                    )

            interrupt_after: int | None = None
            raw_interrupt = os.environ.get(
                "BACKUP_CHAIN_TEST_INTERRUPT_AFTER_BLOCKS"
            )
            if raw_interrupt:
                try:
                    interrupt_after = int(raw_interrupt)
                except ValueError:
                    interrupt_after = None
            committed_count = len(committed)
            expected_output_size = sum(block["length"] for block in blocks)
            events_before = self.store.drill_events(drill_id)

            if committed:
                handle = staging_path.open("r+b")
                handle.seek(progress_bytes)
            else:
                handle = staging_path.open("w+b")
            try:
                for index, block in enumerate(blocks):
                    if index < committed_count:
                        continue
                    path = self.repo / block["path"]
                    if not path.is_file() or path.is_symlink():
                        return self._fail_preflight(
                            drill_id=drill_id,
                            block=block,
                            code=BC_DRILL_BLOCK_MISSING,
                            detail={"message": "selected block file is missing", "path": block["path"]},
                        )
                    offset = handle.tell()
                    digest = hashlib.sha256()
                    read_bytes = 0
                    write_bytes = 0
                    try:
                        with path.open("rb") as source:
                            while True:
                                data = source.read(1024 * 1024)
                                if not data:
                                    break
                                read_bytes += len(data)
                                digest.update(data)
                                handle.write(data)
                                write_bytes += len(data)
                        handle.truncate(handle.tell())
                        handle.flush()
                        os.fsync(handle.fileno())
                    except OSError as exc:
                        try:
                            handle.seek(offset)
                            handle.truncate()
                            handle.flush()
                            os.fsync(handle.fileno())
                        except OSError:
                            pass
                        failed = self.store.fail_drill_block(
                            drill_id=drill_id,
                            block=block,
                            code=BC_DRILL_BLOCK_READ_ERROR,
                            actual_sha256=None,
                            detail={
                                "message": "read-only source block could not be read",
                                "error": str(exc),
                                "path": block["path"],
                            },
                            read_bytes=read_bytes,
                            write_bytes=0,
                        )
                        return self._drill_public(failed) or {}

                    actual_sha = digest.hexdigest()
                    actual_size = write_bytes
                    if actual_sha != block["sha256"] or actual_size != block["length"]:
                        try:
                            handle.seek(offset)
                            handle.truncate()
                            handle.flush()
                            os.fsync(handle.fileno())
                        except OSError:
                            pass
                        failed = self.store.fail_drill_block(
                            drill_id=drill_id,
                            block=block,
                            code=BC_DRILL_BLOCK_DIGEST_MISMATCH,
                            actual_sha256=actual_sha,
                            detail={
                                "actual_length": actual_size,
                                "expected_length": block["length"],
                                "expected_sha256": block["sha256"],
                                "message": "restore block failed its content-digest check",
                                "path": block["path"],
                            },
                            read_bytes=read_bytes,
                            write_bytes=write_bytes,
                        )
                        return self._drill_public(failed) or {}

                    if index == len(blocks) - 1:
                        # Keep the staging file until the completion transaction
                        # succeeds.  The final file is never a partial product.
                        if staging_path.stat().st_size != expected_output_size:
                            failed = self.store.fail_drill_block(
                                drill_id=drill_id,
                                block=block,
                                code=BC_DRILL_OUTPUT_INCOMPLETE,
                                actual_sha256=actual_sha,
                                detail={
                                    "message": "staging size does not match the frozen selection",
                                    "expected_size": expected_output_size,
                                    "actual_size": staging_path.stat().st_size,
                                },
                                read_bytes=read_bytes,
                                write_bytes=write_bytes,
                            )
                            return self._drill_public(failed) or {}
                        final_temp = final_path.with_name(final_path.name + ".tmp")
                        try:
                            shutil.copyfile(staging_path, final_temp)
                            with final_temp.open("r+b") as check_handle:
                                os.fsync(check_handle.fileno())
                            os.replace(final_temp, final_path)
                        except OSError as exc:
                            failed = self.store.fail_drill_block(
                                drill_id=drill_id,
                                block=block,
                                code=BC_DRILL_OUTPUT_INCOMPLETE,
                                actual_sha256=actual_sha,
                                detail={
                                    "message": "final output could not be published",
                                    "error": str(exc),
                                },
                                read_bytes=read_bytes,
                                write_bytes=write_bytes,
                            )
                            return self._drill_public(failed) or {}

                        completed_blocks = [
                            {
                                "actual_sha256": item["actual_sha256"],
                                "block_id": item["block_id"],
                                "length": item["length"],
                                "ordinal": item["ordinal"],
                                "path": item["path"],
                                "sha256": item["sha256"],
                            }
                            for item in committed
                        ]
                        completed_blocks.append(
                            {
                                "actual_sha256": actual_sha,
                                "block_id": block["block_id"],
                                "length": block["length"],
                                "ordinal": block["ordinal"],
                                "path": block["path"],
                                "sha256": block["sha256"],
                            }
                        )
                        completed_blocks.sort(
                            key=lambda item: (item["ordinal"], item["block_id"])
                        )
                        conclusion = {
                            "block_count": len(blocks),
                            "blocks": completed_blocks,
                            "chain_id": audit["chain_id"],
                            "completed_at": utc_now(),
                            "drill_id": drill_id,
                            "output_path": str(final_path),
                            "output_size": sum(
                                item["length"] for item in completed_blocks
                            ),
                            "resume_count": drill["resume_count"],
                            "resume_events": events_before,
                            "restore_point": int(restore_point),
                            "state_digest": point["state_digest"],
                        }
                        completed = self.store.commit_drill_block(
                            drill_id=drill_id,
                            block=block,
                            actual_sha256=actual_sha,
                            read_bytes=read_bytes,
                            write_bytes=write_bytes,
                            complete=True,
                            output_path=str(final_path),
                            conclusion=conclusion,
                        )
                        try:
                            staging_path.unlink()
                        except FileNotFoundError:
                            pass
                        return {
                            **self._drill_public(completed),
                            "idempotent_replay": False,
                        }

                    self.store.commit_drill_block(
                        drill_id=drill_id,
                        block=block,
                        actual_sha256=actual_sha,
                        read_bytes=read_bytes,
                        write_bytes=write_bytes,
                    )
                    committed.append(
                        {
                            "actual_sha256": actual_sha,
                            "block_id": block["block_id"],
                            "length": block["length"],
                            "ordinal": block["ordinal"],
                            "path": block["path"],
                            "sha256": block["sha256"],
                        }
                    )
                    committed_count += 1
                    if (
                        interrupt_after is not None
                        and committed_count >= interrupt_after
                        and committed_count < len(blocks)
                    ):
                        interrupted = self.store.mark_drill_interrupted(
                            drill_id,
                            reason="simulated interruption after committed progress",
                        )
                        return {
                            **self._drill_public(interrupted),
                            "simulated_interrupt": True,
                        }
            finally:
                try:
                    handle.close()
                except OSError:
                    pass
                for stale_path in (
                    final_path.with_name(final_path.name + ".tmp"),
                ):
                    try:
                        stale_path.unlink()
                    except FileNotFoundError:
                        pass
            raise BusinessError(
                BC_DRILL_OUTPUT_INCOMPLETE,
                "restore loop exited before a conclusion was committed",
                drill_id=drill_id,
            )

    def metrics_text(self) -> str:
        rows = self.store.metrics()
        lines = [
            "# HELP backup_chain_increments_total Declared daily increments in the latest committed chain view.",
            "# TYPE backup_chain_increments_total gauge",
            "# HELP backup_chain_usable_restore_points Usable restore points, including the full baseline.",
            "# TYPE backup_chain_usable_restore_points gauge",
            "# HELP backup_chain_conflict_blocks Logical blocks with physical copies of differing content digests.",
            "# TYPE backup_chain_conflict_blocks gauge",
            "# HELP backup_chain_missing_blocks Logical blocks with no declared physical replica.",
            "# TYPE backup_chain_missing_blocks gauge",
            "# HELP backup_chain_half_written_blocks Logical blocks with a length or tail-digest mismatch.",
            "# TYPE backup_chain_half_written_blocks gauge",
            "# HELP backup_chain_committed_drills_total Completed restore drills for the chain.",
            "# TYPE backup_chain_committed_drills_total counter",
        ]

        def label(value: str) -> str:
            return (
                value.replace("\\", "\\\\")
                .replace("\n", "\\n")
                .replace('"', '\\"')
            )

        for row in rows:
            labels = f'backup_id="{label(row["chain_id"])}"'
            lines.append(
                f'backup_chain_increments_total{{{labels}}} '
                f'{row["increment_count"]}'
            )
            lines.append(
                f'backup_chain_usable_restore_points{{{labels}}} '
                f'{row["usable_restore_points"]}'
            )
            lines.append(
                f'backup_chain_conflict_blocks{{{labels}}} '
                f'{row["conflict_blocks"]}'
            )
            lines.append(
                f'backup_chain_missing_blocks{{{labels}}} '
                f'{row["missing_blocks"]}'
            )
            lines.append(
                f'backup_chain_half_written_blocks{{{labels}}} '
                f'{row["half_written_blocks"]}'
            )
            lines.append(
                f'backup_chain_committed_drills_total{{{labels}}} '
                f'{row["committed_drills"]}'
            )
        return "\n".join(lines) + "\n"
