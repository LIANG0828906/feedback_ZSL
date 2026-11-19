from __future__ import annotations

import hashlib
import json
import logging
import math
import threading
import time
import uuid
from collections import Counter, deque
from contextlib import ExitStack
from datetime import datetime, timezone
from typing import Any

from .config import Config
from .errors import AppError, check_unavailable, dual_write_error, not_migrated
from .ledger import Ledger, utc_now
from .locks import OrderLocks, WriteGate
from .model import Order, OrderSummary
from .repository import OrderRepository
from .sqlclient import (
    DatabaseAccessError,
    DatabaseHub,
    shard_key,
    tenant_shard,
)
from .topology import TopologyReport, TopologyValidator


LOG = logging.getLogger("orders_migration")
MAX_ORDER_ID = 9_223_372_036_854_775_807


class InjectedFault(Exception):
    def __init__(self, stage: str):
        self.stage = stage
        super().__init__(f"injected fault at {stage}")


def _p95(samples: list[float]) -> float:
    if not samples:
        return 0.0
    ordered = sorted(samples)
    index = max(0, math.ceil(len(ordered) * 0.95) - 1)
    return round(ordered[index], 3)


class MigrationService:
    def __init__(
        self,
        config: Config,
        hub: DatabaseHub,
        topology: TopologyReport,
        ledger: Ledger,
    ):
        self.config = config
        self.hub = hub
        self.topology = topology
        self.ledger = ledger
        self.repository = OrderRepository(hub)
        self.locks = OrderLocks()
        self.tenant_locks = OrderLocks()
        self.gate = WriteGate()
        self._allocation_lock = threading.Lock()
        self._stats_lock = threading.Lock()
        self._write_latencies: deque[float] = deque(maxlen=10000)
        self._read_latencies: deque[float] = deque(maxlen=10000)
        self._error_counts: Counter[str] = Counter()
        self._fault_lock = threading.Lock()
        self._faults: Counter[str] = Counter()
        self._recovery_threads: list[threading.Thread] = []
        self.started_at = utc_now()
        self.recover_incomplete_dual_writes()

    @classmethod
    def create(cls, config: Config) -> "MigrationService":
        hub = DatabaseHub(config)
        try:
            topology = TopologyValidator(hub, config.shard_count).validate()
        except BaseException:
            hub.close()
            raise
        try:
            ledger = Ledger(config.sqlite_path)
            return cls(config, hub, topology, ledger)
        except BaseException:
            hub.close()
            raise

    def close(self) -> None:
        self.hub.close()
        self.ledger.close()

    # ------------------------------------------------------------------ health

    def health(self) -> dict[str, Any]:
        return {
            "ok": True,
            "service": "orders-migration",
            "ready": True,
            "startedAt": self.started_at,
            "shardCount": self.config.shard_count,
            "schemaSignature": self.topology.schema_signature,
            "connectionPool": self.hub.pool.metrics,
            "writeGate": self.gate.counts,
            "readModeDuringMigration": (
                self.config.migration.read_mode_during_migration
            ),
        }

    def topology_report(self) -> dict[str, Any]:
        return self.topology.as_dict()

    def metrics(self) -> dict[str, Any]:
        with self._stats_lock:
            writes = list(self._write_latencies)
            reads = list(self._read_latencies)
            errors = dict(self._error_counts)
        return {
            "write": {
                "samples": len(writes),
                "p95Ms": _p95(writes),
                "maxMs": round(max(writes), 3) if writes else 0,
                "targetP95Ms": self.config.load_limits.write_p95_ms,
            },
            "read": {
                "samples": len(reads),
                "p95Ms": _p95(reads),
                "maxMs": round(max(reads), 3) if reads else 0,
                "targetP95Ms": self.config.load_limits.read_p95_ms,
            },
            "errors": errors,
            "connectionPool": self.hub.pool.metrics,
            "writeGate": self.gate.counts,
        }

    def record_error(self, code: str) -> None:
        with self._stats_lock:
            self._error_counts[code] += 1

    def _latency(self, kind: str, started: float) -> None:
        elapsed = (time.monotonic() - started) * 1000.0
        with self._stats_lock:
            if kind == "write":
                self._write_latencies.append(elapsed)
            else:
                self._read_latencies.append(elapsed)

    def set_fault(self, stage: str, times: int = 1) -> dict[str, Any]:
        allowed = {
            "before_shard_commit",
            "after_shard_commit",
            "before_legacy_commit",
            "after_shard_batch_commit",
            "during_check",
            "after_rollback_batch",
        }
        if stage not in allowed:
            raise AppError(
                "E_VALIDATION",
                f"unknown fault stage {stage}",
                {"allowed": sorted(allowed)},
                400,
            )
        with self._fault_lock:
            self._faults[stage] += max(0, times)
            return {"faults": dict(self._faults)}

    def _fault(self, stage: str) -> None:
        with self._fault_lock:
            if self._faults[stage] > 0:
                self._faults[stage] -= 1
                raise InjectedFault(stage)

    # ------------------------------------------------------------------ routing

    def _route_for(self, tenant_id: int) -> dict[str, Any]:
        state = self.ledger.migration(tenant_id)
        status = str(state["status"]) if state else "NOT_STARTED"
        current = self.ledger.route(tenant_id)
        if current and current["mode"] == "rolled_back":
            return {
                "database": "legacy",
                "mode": "legacy",
                "rank": int(current["rank"]),
                "generation": int(current["generation"]),
                "migrationStatus": status,
                "shard": tenant_shard(tenant_id, self.config.shard_count),
            }

        if status == "COMPLETE":
            target_mode, target_rank = "shard", 3
        elif status == "IN_PROGRESS":
            if self.config.migration.read_mode_during_migration == "shard":
                target_mode, target_rank = "shard", 2
            else:
                target_mode, target_rank = "legacy", 1
        else:
            target_mode, target_rank = "legacy", 0

        if current and int(current["rank"]) > target_rank:
            target_mode = str(current["mode"])
            target_rank = int(current["rank"])
        elif current is None or target_rank > int(current["rank"]):
            current = self.ledger.advance_route(
                tenant_id, target_mode, target_rank
            )

        return {
            "database": "legacy" if target_mode == "legacy" else shard_key(
                tenant_shard(tenant_id, self.config.shard_count)
            ),
            "mode": target_mode,
            "rank": target_rank,
            "generation": int(current["generation"]) if current else 1,
            "migrationStatus": status,
            "shard": tenant_shard(tenant_id, self.config.shard_count),
        }

    def _advance_forward_route(
        self, tenant_id: int, status: str, *, force_rank: int | None = None
    ) -> dict[str, Any]:
        current = self.ledger.route(tenant_id)
        if current and current["mode"] == "rolled_back":
            return current
        if force_rank is not None:
            mode = "shard" if force_rank >= 2 else "legacy"
            return self.ledger.advance_route(tenant_id, mode, force_rank)
        if status == "COMPLETE":
            return self.ledger.advance_route(tenant_id, "shard", 3)
        if self.config.migration.read_mode_during_migration == "shard":
            return self.ledger.advance_route(tenant_id, "shard", 2)
        return self.ledger.advance_route(tenant_id, "legacy", 1)

    # ------------------------------------------------------------------- writes

    def write_order(
        self, payload: dict[str, Any], idempotency_key: str | None = None
    ) -> dict[str, Any]:
        started = time.monotonic()
        self.gate.enter_write()
        allocation_held = False
        order_id: int | None = None
        try:
            raw = dict(payload)
            if "orderId" not in raw or raw.get("orderId") in (None, ""):
                self._allocation_lock.acquire()
                allocation_held = True
                raw["orderId"] = self.repository.next_order_id()
            order_id = int(raw["orderId"])
            tenant_value = raw.get("tenantId")

            with self.locks.acquire(order_id, timeout=30):
                legacy_before = self.repository.fetch_order("legacy", order_id)
                if tenant_value is None and legacy_before is not None:
                    raw["tenantId"] = legacy_before.tenant_id
                if legacy_before is not None:
                    raw.setdefault("createdAt", legacy_before.created_at.isoformat())
                    raw.setdefault("status", legacy_before.status)
                    raw.setdefault("amount", format(legacy_before.amount, ".2f"))
                    raw.setdefault("currency", legacy_before.currency)
                    if not raw.get("items"):
                        raw["items"] = [
                            item.as_dict() for item in legacy_before.items
                        ]
                parsed = Order.from_payload(raw)
                shard = shard_key(
                    tenant_shard(parsed.tenant_id, self.config.shard_count)
                )
                shard_before = self.repository.fetch_order(shard, order_id)

                existing = legacy_before or shard_before
                if existing is not None and existing.tenant_id != parsed.tenant_id:
                    raise AppError(
                        "E_VALIDATION",
                        "tenantId cannot change for an existing order",
                        {
                            "orderId": order_id,
                            "existingTenantId": existing.tenant_id,
                            "requestedTenantId": parsed.tenant_id,
                        },
                        400,
                    )
                if existing is not None:
                    if not raw.get("items"):
                        raw["items"] = [item.as_dict() for item in existing.items]
                    raw.setdefault("createdAt", existing.created_at.isoformat())
                    raw.setdefault("status", existing.status)
                    raw.setdefault("amount", format(existing.amount, ".2f"))
                    raw.setdefault("currency", existing.currency)
                    if "updatedAt" not in raw:
                        raw["updatedAt"] = utc_now()
                    parsed = Order.from_payload(raw)

                revision_candidates = [
                    value.revision
                    for value in (legacy_before, shard_before)
                    if value is not None
                ]
                revision = max(revision_candidates + [parsed.revision, 0]) + 1
                order = parsed.with_revision_and_time(
                    revision, parsed.updated_at
                )
                digest = order.payload_hash()
                if idempotency_key:
                    write_id = hashlib.sha256(
                        f"{idempotency_key}:{order_id}:{digest}".encode("utf-8")
                    ).hexdigest()
                else:
                    write_id = uuid.uuid4().hex

                previous = (
                    self.ledger.dual_write(write_id)
                    if idempotency_key
                    else None
                )
                if previous and previous["status"] == "COMMITTED":
                    self._latency("write", started)
                    return {
                        "ok": True,
                        "idempotentReplay": True,
                        "writeId": write_id,
                        "orderId": order_id,
                        "revision": previous["revision"],
                        "legacyCommitSite": previous["legacy_commit_site"],
                        "shardCommitSite": previous["shard_commit_site"],
                        "order": order.as_dict(include_items=True),
                    }
                if previous:
                    raise dual_write_error(
                        "the idempotent request has an unfinished or failed attempt",
                        writeId=write_id,
                        orderId=order_id,
                        previousStatus=previous["status"],
                        previousError=previous["error_code"],
                    )

                self.ledger.record_dual_intent(
                    write_id=write_id,
                    order_id=order_id,
                    tenant_id=order.tenant_id,
                    revision=revision,
                    payload_hash=digest,
                    legacy_before=(
                        legacy_before.as_dict(include_items=True)
                        if legacy_before
                        else None
                    ),
                    shard_before=(
                        shard_before.as_dict(include_items=True)
                        if shard_before
                        else None
                    ),
                )

                shard_receipt = {
                    "writeId": write_id,
                    "kind": "DUAL_WRITE",
                    "payloadHash": digest,
                    "committedAt": datetime.now(timezone.utc),
                }
                try:
                    self._fault("before_shard_commit")
                    with self.hub.transaction(shard) as session:
                        self.repository.write_order(
                            session,
                            order,
                            receipt=shard_receipt,
                            known_exists=shard_before is not None,
                            check_receipt=False,
                        )
                    shard_site = (
                        f"{self.hub.database_name(shard)}:{write_id}:"
                        f"{utc_now()}"
                    )
                    self._fault("after_shard_commit")
                except BaseException as exc:
                    committed_receipt = self.repository.receipt(shard, write_id)
                    ambiguous_commit = (
                        not isinstance(exc, InjectedFault)
                        and committed_receipt is not None
                        and committed_receipt["payloadHash"] == digest
                    )
                    if ambiguous_commit:
                        shard_site = (
                            f"{self.hub.database_name(shard)}:{write_id}:recovered"
                        )
                        LOG.warning(
                            "shard commit for %s was ambiguous but receipt exists",
                            write_id,
                        )
                    else:
                        self._compensate_after_failure(
                            write_id, order_id, shard, shard_before
                        )
                        message = (
                            "shard side failed at "
                            f"{getattr(exc, 'stage', 'commit')}: {exc}"
                        )
                        self.ledger.fail_dual(
                            write_id,
                            code="E_DUAL_WRITE",
                            message=message,
                            details={"stage": "shard", "compensated": True},
                        )
                        raise dual_write_error(
                            message,
                            writeId=write_id,
                            orderId=order_id,
                            stage="shard",
                            shardRolledBack=True,
                        ) from exc

                legacy_receipt = {
                    "writeId": write_id,
                    "kind": "DUAL_WRITE",
                    "payloadHash": digest,
                    "committedAt": datetime.now(timezone.utc),
                }
                try:
                    self._fault("before_legacy_commit")
                    with self.hub.transaction("legacy") as session:
                        self.repository.write_order(
                            session,
                            order,
                            receipt=legacy_receipt,
                            known_exists=legacy_before is not None,
                            check_receipt=False,
                        )
                    legacy_site = (
                        f"{self.hub.database_name('legacy')}:{write_id}:"
                        f"{utc_now()}"
                    )
                except BaseException as exc:
                    committed_receipt = self.repository.receipt(
                        "legacy", write_id
                    )
                    if (
                        not isinstance(exc, InjectedFault)
                        and committed_receipt is not None
                        and committed_receipt["payloadHash"] == digest
                    ):
                        legacy_site = (
                            f"{self.hub.database_name('legacy')}:"
                            f"{write_id}:recovered"
                        )
                        self.ledger.complete_dual(
                            write_id,
                            legacy_commit_site=legacy_site,
                            shard_commit_site=shard_site,
                        )
                        self._latency("write", started)
                        return {
                            "ok": True,
                            "writeId": write_id,
                            "orderId": order_id,
                            "revision": revision,
                            "legacyCommitSite": legacy_site,
                            "shardCommitSite": shard_site,
                            "recoveredAfterAmbiguousCommit": True,
                            "order": order.as_dict(include_items=True),
                        }
                    compensation_ok = self._compensate_after_failure(
                        write_id, order_id, shard, shard_before
                    )
                    message = f"legacy side failed after shard commit: {exc}"
                    self.ledger.fail_dual(
                        write_id,
                        code="E_DUAL_WRITE",
                        message=message,
                        details={
                            "stage": "legacy",
                            "shardRolledBack": compensation_ok,
                        },
                    )
                    raise dual_write_error(
                        message,
                        writeId=write_id,
                        orderId=order_id,
                        stage="legacy",
                        shardRolledBack=compensation_ok,
                    ) from exc

                self.ledger.complete_dual(
                    write_id,
                    legacy_commit_site=legacy_site,
                    shard_commit_site=shard_site,
                )
                self._latency("write", started)
                return {
                    "ok": True,
                    "writeId": write_id,
                    "orderId": order_id,
                    "revision": revision,
                    "legacyCommitSite": legacy_site,
                    "shardCommitSite": shard_site,
                    "order": order.as_dict(include_items=True),
                }
        except AppError:
            raise
        except DatabaseAccessError as exc:
            raise dual_write_error(
                "database access failed during dual write",
                stage="legacy" if exc.database == "legacy" else "shard",
                database=exc.database,
                orderId=order_id,
                detail=str(exc.original),
            ) from exc
        finally:
            if allocation_held:
                self._allocation_lock.release()
            self.gate.leave_write()

    def _compensate_after_failure(
        self,
        write_id: str,
        order_id: int,
        database: str,
        before: Order | None,
    ) -> bool:
        for attempt in range(3):
            try:
                with self.hub.transaction(database) as session:
                    if before is None:
                        self.repository.delete_order(session, order_id)
                    else:
                        self.repository.write_order(
                            session,
                            before,
                            receipt={
                                "writeId": f"comp:{write_id}"[:96],
                                "kind": "COMPENSATION",
                                "payloadHash": before.payload_hash(),
                            },
                            known_exists=True,
                        )
                return True
            except Exception as exc:
                LOG.error(
                    "compensation attempt %s for %s failed: %s",
                    attempt + 1,
                    write_id,
                    exc,
                )
                time.sleep(0.05 * (attempt + 1))
        return False

    # ------------------------------------------------------------------- query

    def get_order(self, order_id: int, tenant_id: int) -> dict[str, Any]:
        started = time.monotonic()
        with self.locks.acquire(order_id, timeout=30):
            route = self._route_for(tenant_id)
            database = route["database"]
            try:
                order = self.repository.fetch_order(database, order_id)
            except DatabaseAccessError as exc:
                raise AppError(
                    "E_QUERY_UNAVAILABLE",
                    "database access failed while reading order",
                    {"database": database, "orderId": order_id},
                    503,
                ) from exc
            if order is None:
                if database != "legacy":
                    raise not_migrated(order_id, tenant_id)
                raise AppError(
                    "E_NOT_FOUND",
                    f"order {order_id} was not found",
                    {"orderId": order_id, "tenantId": tenant_id},
                    404,
                )
            self._latency("read", started)
            return {
                "ok": True,
                "route": {
                    "database": database,
                    "mode": route["mode"],
                    "generation": route["generation"],
                    "migrationStatus": route["migrationStatus"],
                    "shard": route["shard"],
                },
                "order": order.as_dict(include_items=True),
            }

    # --------------------------------------------------------------- migration

    def migration_status(self, tenant_id: int) -> dict[str, Any]:
        state = self.ledger.migration(tenant_id)
        route = self._route_for(tenant_id)
        if state is None:
            return {
                "ok": True,
                "tenantId": tenant_id,
                "shard": tenant_shard(tenant_id, self.config.shard_count),
                "status": "NOT_STARTED",
                "lastOrderId": 0,
                "lastCommitSite": None,
                "route": route,
            }
        return {
            "ok": True,
            "tenantId": tenant_id,
            "shard": int(state["shard_index"]),
            "status": state["status"],
            "lastOrderId": int(state["last_order_id"]),
            "lastCommitSite": state["last_commit_site"],
            "generation": int(state["generation"]),
            "error": (
                {"code": state["error_code"], "message": state["error_message"]}
                if state["error_code"]
                else None
            ),
            "route": route,
        }

    def migrate_tenant(
        self, tenant_id: int, max_batches: int | None = None
    ) -> dict[str, Any]:
        if tenant_id < 0:
            raise AppError(
                "E_VALIDATION", "tenantId must be non-negative", http_status=400
            )
        shard_index = tenant_shard(tenant_id, self.config.shard_count)
        shard = shard_key(shard_index)
        with self.tenant_locks.acquire(tenant_id):
            self.gate.enter_migration()
            processed_batches = 0
            try:
                state = self.ledger.begin_migration(tenant_id, shard_index)
                self._advance_forward_route(tenant_id, "IN_PROGRESS")
                if state["status"] == "COMPLETE":
                    return {
                        "ok": True,
                        "tenantId": tenant_id,
                        "shard": shard_index,
                        "status": "COMPLETE",
                        "lastOrderId": int(state["last_order_id"]),
                        "processedBatches": 0,
                    }
                cursor = int(state["last_order_id"])
                while True:
                    if self.gate.stopping:
                        raise AppError(
                            "E_WRITE_STOPPED",
                            "migration paused because rollback is draining writes",
                            {"tenantId": tenant_id, "lastOrderId": cursor},
                            503,
                        )
                    try:
                        orders = self.repository.fetch_orders_for_tenant(
                            "legacy",
                            tenant_id,
                            cursor,
                            self.config.migration.batch_size,
                        )
                    except DatabaseAccessError as exc:
                        raise AppError(
                            "E_MIGRATION",
                            "legacy read failed during migration",
                            {"tenantId": tenant_id, "lastOrderId": cursor},
                            503,
                        ) from exc
                    if not orders:
                        site = self.repository.receipt_max_time(
                            shard, tenant_id
                        )
                        self.ledger.complete_migration(
                            tenant_id, cursor, site
                        )
                        self._advance_forward_route(tenant_id, "COMPLETE")
                        return {
                            "ok": True,
                            "tenantId": tenant_id,
                            "shard": shard_index,
                            "status": "COMPLETE",
                            "lastOrderId": cursor,
                            "lastCommitSite": site,
                            "processedBatches": processed_batches,
                        }

                    ordered_ids = sorted(order.order_id for order in orders)
                    with ExitStack() as stack:
                        for order_id in ordered_ids:
                            stack.enter_context(
                                self.locks.acquire(order_id, timeout=30)
                            )
                        try:
                            with self.hub.transaction(shard) as session:
                                for order in orders:
                                    self.repository.write_order(
                                        session,
                                        order,
                                        receipt={
                                            "writeId": (
                                                f"mig:{tenant_id}:{order.order_id}:"
                                                f"{order.revision}"
                                            )[:96],
                                            "kind": "MIGRATION",
                                            "payloadHash": order.payload_hash(),
                                            "committedAt": order.updated_at,
                                        },
                                        conditional=True,
                                    )
                        except DatabaseAccessError as exc:
                            raise AppError(
                                "E_MIGRATION",
                                "shard write failed during migration",
                                {"tenantId": tenant_id, "lastOrderId": cursor},
                                503,
                            ) from exc
                    try:
                        self._fault("after_shard_batch_commit")
                    except InjectedFault as exc:
                        self.ledger.fail_migration(
                            tenant_id, "E_MIGRATION_INTERRUPTED", str(exc)
                        )
                        raise AppError(
                            "E_MIGRATION_INTERRUPTED",
                            "migration interrupted after a committed shard batch",
                            {
                                "tenantId": tenant_id,
                                "lastOrderId": cursor,
                                "committedThrough": orders[-1].order_id,
                            },
                            409,
                        ) from exc

                    next_cursor = orders[-1].order_id
                    site = self.repository.receipt_max_time(shard, tenant_id)
                    self.ledger.record_migration_batch(
                        tenant_id=tenant_id,
                        from_order_id=cursor + 1,
                        through_order_id=next_cursor,
                        last_order_id=next_cursor,
                        commit_site=site,
                        order_count=len(orders),
                    )
                    cursor = next_cursor
                    processed_batches += 1
                    self._advance_forward_route(tenant_id, "IN_PROGRESS")
                    if max_batches is not None and processed_batches >= max_batches:
                        return {
                            "ok": True,
                            "tenantId": tenant_id,
                            "shard": shard_index,
                            "status": "IN_PROGRESS",
                            "lastOrderId": cursor,
                            "lastCommitSite": site,
                            "processedBatches": processed_batches,
                        }
            except AppError:
                raise
            except Exception as exc:
                self.ledger.fail_migration(
                    tenant_id, "E_MIGRATION", str(exc)
                )
                raise AppError(
                    "E_MIGRATION",
                    "migration failed; committed progress was retained",
                    {"tenantId": tenant_id, "detail": str(exc)},
                    503,
                ) from exc
            finally:
                self.gate.leave_migration()

    # ---------------------------------------------------------- reconciliation

    def check(
        self,
        *,
        order_id_start: int = 0,
        order_id_end: int = MAX_ORDER_ID,
        tenant_id: int | None = None,
        force: bool = False,
        run_id: str | None = None,
    ) -> dict[str, Any]:
        if order_id_start < 0 or order_id_end < order_id_start:
            raise AppError(
                "E_VALIDATION",
                "order range is invalid",
                {"orderIdStart": order_id_start, "orderIdEnd": order_id_end},
                400,
            )
        scope = {
            "orderIdStart": order_id_start,
            "orderIdEnd": order_id_end,
            "tenantId": tenant_id,
        }
        if run_id is None:
            scope_key = hashlib.sha256(
                json.dumps(scope, sort_keys=True).encode("utf-8")
            ).hexdigest()
            new_run_id = uuid.uuid4().hex
            run, created = self.ledger.create_check_run(
                run_id=new_run_id,
                scope_key=scope_key,
                scope=scope,
                force=force,
            )
            if not created:
                return self._check_response(run)
        else:
            run = self.ledger.check_run(run_id)
            if run is None:
                raise AppError(
                    "E_VALIDATION",
                    "check run does not exist",
                    {"runId": run_id},
                    404,
                )
            if run["status"] == "COMPLETE":
                return self._check_response(run)
            scope = json.loads(run["scope_json"])
        return self._continue_check(run, scope)

    def _continue_check(
        self, run: dict[str, Any], scope: dict[str, Any]
    ) -> dict[str, Any]:
        run_id = str(run["run_id"])
        start = int(scope.get("orderIdStart", 0))
        end = int(scope.get("orderIdEnd", MAX_ORDER_ID))
        tenant_id = scope.get("tenantId")
        cursor = max(start, int(run["cursor_order_id"]))
        total_checked = int(run["total_checked"])
        try:
            while True:
                order_ids = self._next_union_ids(
                    cursor,
                    end,
                    tenant_id,
                    self.config.reconciliation.batch_size,
                )
                if not order_ids:
                    self.ledger.complete_check(
                        run_id,
                        cursor_order_id=cursor,
                        total_checked=total_checked,
                    )
                    break
                with ExitStack() as stack:
                    for order_id in order_ids:
                        stack.enter_context(
                            self.locks.acquire(order_id, timeout=30)
                        )
                    legacy = self.repository.summaries("legacy", order_ids)
                    shard_summaries: dict[int, OrderSummary] = {}
                    for shard in range(self.config.shard_count):
                        shard_summaries.update(
                            self.repository.summaries(
                                shard_key(shard), order_ids
                            )
                        )
                    mismatches = self._compare_batch(
                        order_ids, legacy, shard_summaries
                    )
                cursor = max(order_ids)
                total_checked += len(order_ids)
                self.ledger.append_check_batch(
                    run_id=run_id,
                    cursor_order_id=cursor,
                    total_checked=total_checked,
                    mismatches=mismatches,
                )
                self._fault("during_check")
            updated = self.ledger.check_run(run_id)
            assert updated is not None
            return self._check_response(updated)
        except AppError:
            raise
        except DatabaseAccessError as exc:
            self.ledger.fail_check(
                run_id,
                "E_CHECK_UNAVAILABLE",
                f"{exc.database}: {exc.original}",
            )
            raise check_unavailable(
                "reconciliation could not reach one database; partial results retained",
                runId=run_id,
                cursorOrderId=cursor,
                completedCount=total_checked,
                database=exc.database,
                resumeAvailable=True,
            ) from exc
        except Exception as exc:
            self.ledger.fail_check(run_id, "E_CHECK_UNAVAILABLE", str(exc))
            raise check_unavailable(
                "reconciliation stopped unexpectedly; partial results retained",
                runId=run_id,
                cursorOrderId=cursor,
                completedCount=total_checked,
                detail=str(exc),
                resumeAvailable=True,
            ) from exc

    def _next_union_ids(
        self,
        cursor: int,
        end: int,
        tenant_id: int | None,
        limit: int,
    ) -> list[int]:
        ids = set(
            self.repository.order_ids_between(
                "legacy",
                cursor,
                end,
                limit,
                tenant_id=tenant_id,
            )
        )
        shards = (
            [tenant_shard(tenant_id, self.config.shard_count)]
            if tenant_id is not None
            else list(range(self.config.shard_count))
        )
        for shard in shards:
            ids.update(
                self.repository.order_ids_between(
                    shard_key(shard),
                    cursor,
                    end,
                    limit,
                    tenant_id=tenant_id,
                )
            )
        return sorted(ids)[:limit]

    def _compare_batch(
        self,
        order_ids: list[int],
        legacy: dict[int, OrderSummary],
        shards: dict[int, OrderSummary],
    ) -> list[dict[str, Any]]:
        mismatches: list[dict[str, Any]] = []
        for order_id in order_ids:
            left = legacy.get(order_id)
            right = shards.get(order_id)
            if left is None and right is None:
                continue
            if left is None or right is None:
                mismatches.append(
                    {
                        "orderId": order_id,
                        "tenantId": (
                            right.tenant_id if right else left.tenant_id
                            if left
                            else None
                        ),
                        "fieldName": "order",
                        "legacyValue": "present" if left else "missing",
                        "shardValue": "present" if right else "missing",
                    }
                )
                continue
            left_values = left.comparable()
            right_values = right.comparable()
            for field in (
                "tenantId",
                "status",
                "amount",
                "currency",
                "itemCount",
                "itemFingerprint",
                "createdAt",
                "updatedAt",
                "revision",
            ):
                if left_values[field] != right_values[field]:
                    mismatches.append(
                        {
                            "orderId": order_id,
                            "tenantId": left.tenant_id,
                            "fieldName": field,
                            "legacyValue": left_values[field],
                            "shardValue": right_values[field],
                        }
                    )
        return mismatches

    def _check_response(self, run: dict[str, Any]) -> dict[str, Any]:
        results = self.ledger.check_results(str(run["run_id"]), limit=10000)
        scope = json.loads(run["scope_json"])
        return {
            "ok": run["status"] == "COMPLETE",
            "runId": run["run_id"],
            "status": run["status"],
            "scope": scope,
            "cursorOrderId": int(run["cursor_order_id"]),
            "totalChecked": int(run["total_checked"]),
            "mismatchCount": int(run["mismatch_count"]),
            "mismatches": [
                json.loads(row["detail_json"]) for row in results
            ],
            "error": (
                {"code": run["error_code"], "message": run["error_message"]}
                if run["error_code"]
                else None
            ),
        }

    def check_status(self, run_id: str) -> dict[str, Any]:
        run = self.ledger.check_run(run_id)
        if run is None:
            raise AppError(
                "E_VALIDATION",
                "check run does not exist",
                {"runId": run_id},
                404,
            )
        return self._check_response(run)

    # ----------------------------------------------------------------- rollback

    def _all_tenants(self) -> list[int]:
        try:
            legacy = set(self.repository.distinct_tenants("legacy"))
        except DatabaseAccessError as exc:
            raise AppError(
                "E_ROLLBACK",
                "could not enumerate legacy tenants",
                {"database": exc.database, "detail": str(exc.original)},
                409,
            ) from exc
        return sorted(legacy | set(self.ledger.migration_tenants()))

    def rollback(
        self,
        *,
        tenants: list[int] | None = None,
        run_id: str | None = None,
    ) -> dict[str, Any]:
        if run_id is None:
            drain = self.gate.drain(self.config.rollback.wait_timeout_seconds)
            try:
                pending = (
                    sorted(set(int(value) for value in tenants))
                    if tenants
                    else self._all_tenants()
                )
            except Exception as exc:
                self.gate.resume_writes()
                raise AppError(
                    "E_ROLLBACK",
                    "could not enumerate tenants for rollback",
                    {"detail": str(exc)},
                    409,
                ) from exc
            new_run_id = uuid.uuid4().hex
            run = self.ledger.create_rollback_run(
                run_id=new_run_id,
                scope={"tenants": pending or "all"},
                pending=pending,
            )
            run_id = new_run_id
        else:
            run = self.ledger.rollback_run(run_id)
            if run is None:
                raise AppError(
                    "E_VALIDATION",
                    "rollback run does not exist",
                    {"runId": run_id},
                    404,
                )
            if run["status"] == "COMPLETE":
                self.gate.resume_writes()
                return self._rollback_response(run_id)
            drain = self.gate.drain(self.config.rollback.wait_timeout_seconds)
            pending = [
                int(value)
                for value in json.loads(run["pending_tenants_json"])
            ]

        current_tenant: int | None = None
        try:
            for tenant_id in pending:
                current_tenant = tenant_id
                progress_rows = {
                    int(row["tenant_id"]): row
                    for row in self.ledger.rollback_progress(run_id)
                }
                progress = progress_rows.get(tenant_id)
                if progress and progress["status"] == "COMPLETE":
                    continue
                self.ledger.set_rollback_tenant_status(
                    run_id, tenant_id, "IN_PROGRESS"
                )
                shard_index = tenant_shard(
                    tenant_id, self.config.shard_count
                )
                shard = shard_key(shard_index)
                cursor = int(progress["last_order_id"]) if progress else 0
                while True:
                    order_ids = self._tenant_batch_ids(
                        cursor,
                        tenant_id,
                        shard,
                        self.config.rollback.batch_size,
                    )
                    if not order_ids:
                        break
                    with ExitStack() as stack:
                        for order_id in order_ids:
                            stack.enter_context(
                                self.locks.acquire(order_id, timeout=30)
                            )
                        legacy_orders = self.repository.fetch_orders_by_ids(
                            "legacy", order_ids
                        )
                        shard_orders = self.repository.fetch_orders_by_ids(
                            shard, order_ids
                        )
                        actions, targets = self._rollback_plan(
                            run_id, tenant_id, shard, order_ids,
                            legacy_orders, shard_orders,
                        )
                        for database, changes in targets.items():
                            if not changes:
                                continue
                            with self.hub.transaction(database) as session:
                                for change in changes:
                                    source_order: Order = change["order"]
                                    self.repository.write_order(
                                        session,
                                        source_order,
                                        receipt={
                                            "writeId": change["writeId"],
                                            "kind": "ROLLBACK",
                                            "payloadHash": (
                                                source_order.payload_hash()
                                            ),
                                            "committedAt": source_order.updated_at,
                                        },
                                    )
                        self.ledger.record_rollback_batch(
                            run_id=run_id,
                            tenant_id=tenant_id,
                            actions=actions,
                            cursor_order_id=max(order_ids),
                        )
                        self._fault("after_rollback_batch")
                    cursor = max(order_ids)

                verification = self.check(
                    order_id_start=0,
                    order_id_end=MAX_ORDER_ID,
                    tenant_id=tenant_id,
                    force=True,
                )
                if verification["mismatchCount"]:
                    self.ledger.set_rollback_tenant_status(
                        run_id, tenant_id, "FAILED"
                    )
                    raise AppError(
                        "E_ROLLBACK",
                        "post-rollback verification found differences",
                        {
                            "runId": run_id,
                            "tenantId": tenant_id,
                            "checkRunId": verification["runId"],
                            "mismatchCount": verification["mismatchCount"],
                            "mismatches": verification["mismatches"][:50],
                        },
                        409,
                    )
                self.ledger.finish_rollback_tenant(
                    run_id, tenant_id, cursor
                )
                self.ledger.advance_route(
                    tenant_id, "rolled_back", 100, force=True
                )

            self.ledger.finish_rollback(run_id, status="COMPLETE")
            self.gate.resume_writes()
            return self._rollback_response(run_id)
        except AppError:
            current = self.ledger.rollback_run(run_id) or {}
            pending_now = json.loads(
                current.get("pending_tenants_json", "[]")
            )
            completed = json.loads(
                current.get("completed_tenants_json", "[]")
            )
            failed = [current_tenant] if current_tenant is not None else []
            self.ledger.finish_rollback(
                run_id,
                status="INTERRUPTED",
                code="E_ROLLBACK",
                message="rollback interrupted; write gate remains closed",
                failed_tenants=failed,
            )
            raise AppError(
                "E_ROLLBACK",
                "controlled rollback did not complete",
                {
                    "runId": run_id,
                    "completedTenants": completed,
                    "pendingTenants": pending_now,
                    "failedTenants": failed,
                    "drained": drain,
                    "writeGate": self.gate.counts,
                },
                409,
            )
        except Exception as exc:
            current = self.ledger.rollback_run(run_id) or {}
            pending_now = json.loads(
                current.get("pending_tenants_json", "[]")
            )
            completed = json.loads(
                current.get("completed_tenants_json", "[]")
            )
            failed = [current_tenant] if current_tenant is not None else []
            self.ledger.finish_rollback(
                run_id,
                status="INTERRUPTED",
                code="E_ROLLBACK",
                message=str(exc),
                failed_tenants=failed,
            )
            raise AppError(
                "E_ROLLBACK",
                "controlled rollback failed; partial actions are durable",
                {
                    "runId": run_id,
                    "completedTenants": completed,
                    "pendingTenants": pending_now,
                    "failedTenants": failed,
                    "drained": drain,
                    "detail": str(exc),
                    "writeGate": self.gate.counts,
                },
                409,
            ) from exc

    def _tenant_batch_ids(
        self, cursor: int, tenant_id: int, shard: str, limit: int
    ) -> list[int]:
        ids = set(
            self.repository.order_ids_between(
                "legacy",
                cursor,
                MAX_ORDER_ID,
                limit,
                tenant_id=tenant_id,
            )
        )
        ids.update(
            self.repository.order_ids_between(
                shard,
                cursor,
                MAX_ORDER_ID,
                limit,
                tenant_id=tenant_id,
            )
        )
        return sorted(ids)[:limit]

    def _rollback_plan(
        self,
        run_id: str,
        tenant_id: int,
        shard_database: str,
        order_ids: list[int],
        legacy_orders: dict[int, Order],
        shard_orders: dict[int, Order],
    ) -> tuple[list[dict[str, Any]], dict[str, list[dict[str, Any]]]]:
        actions: list[dict[str, Any]] = []
        targets: dict[str, list[dict[str, Any]]] = {
            "legacy": [],
            shard_database: [],
        }
        for order_id in order_ids:
            legacy = legacy_orders.get(order_id)
            sharded = shard_orders.get(order_id)
            source: Order | None = None
            target_database: str | None = None
            source_name = ""
            action = ""
            if legacy is None and sharded is not None:
                source, target_database = sharded, "legacy"
                source_name, action = "shard", "COPY_SHARD_TO_LEGACY"
            elif sharded is None and legacy is not None:
                source, target_database = legacy, shard_database
                source_name, action = "legacy", "COPY_LEGACY_TO_SHARD"
            elif legacy is not None and sharded is not None:
                if legacy.updated_at < sharded.updated_at:
                    source, target_database = sharded, "legacy"
                    source_name, action = "shard", "COPY_SHARD_TO_LEGACY"
                elif legacy.updated_at > sharded.updated_at:
                    source, target_database = legacy, shard_database
                    source_name, action = "legacy", "COPY_LEGACY_TO_SHARD"
                elif legacy.revision < sharded.revision:
                    source, target_database = sharded, "legacy"
                    source_name, action = "shard", "COPY_SHARD_TO_LEGACY"
                elif legacy.revision > sharded.revision:
                    source, target_database = legacy, shard_database
                    source_name, action = "legacy", "COPY_LEGACY_TO_SHARD"
                elif legacy.payload_hash() != sharded.payload_hash():
                    source, target_database = sharded, "legacy"
                    source_name, action = "shard", "COPY_SHARD_TO_LEGACY"
            if source is None or target_database is None:
                continue
            write_id = (
                f"rb:{run_id[:16]}:{order_id}:{source.revision}:"
                f"{target_database}"
            )[:96]
            targets[target_database].append(
                {"order": source, "writeId": write_id}
            )
            actions.append(
                {
                    "orderId": order_id,
                    "action": action,
                    "source": source_name,
                    "detail": {
                        "target": target_database,
                        "revision": source.revision,
                        "updatedAt": source.updated_at.isoformat(),
                    },
                }
            )
        return actions, targets

    def _rollback_response(self, run_id: str) -> dict[str, Any]:
        run = self.ledger.rollback_run(run_id)
        if run is None:
            raise AppError(
                "E_VALIDATION",
                "rollback run does not exist",
                {"runId": run_id},
                404,
            )
        progress = self.ledger.rollback_progress(run_id)
        return {
            "ok": run["status"] == "COMPLETE",
            "runId": run_id,
            "status": run["status"],
            "scope": json.loads(run["scope_json"]),
            "completedTenants": json.loads(
                run["completed_tenants_json"]
            ),
            "pendingTenants": json.loads(run["pending_tenants_json"]),
            "failedTenants": json.loads(run["failed_tenants_json"]),
            "tenants": progress,
            "error": (
                {"code": run["error_code"], "message": run["error_message"]}
                if run["error_code"]
                else None
            ),
            "writeGate": self.gate.counts,
        }

    def rollback_status(self, run_id: str) -> dict[str, Any]:
        return self._rollback_response(run_id)

    # ----------------------------------------------------------------- recovery

    def recover_incomplete_dual_writes(self) -> None:
        for intent in self.ledger.pending_dual_intents():
            write_id = str(intent["write_id"])
            try:
                legacy = self.repository.receipt("legacy", write_id)
                shard_index = tenant_shard(
                    int(intent["tenant_id"]), self.config.shard_count
                )
                shard = shard_key(shard_index)
                sharded = self.repository.receipt(shard, write_id)
                if legacy and sharded:
                    self.ledger.complete_dual(
                        write_id,
                        legacy_commit_site=f"legacy:{write_id}:recovered",
                        shard_commit_site=f"{shard}:{write_id}:recovered",
                    )
                    continue
                if legacy is None and sharded is None:
                    self.ledger.mark_dual_recovered(
                        write_id, "FAILED", "no side committed; recovered"
                    )
                    continue
                if sharded is not None:
                    before = (
                        json.loads(intent["shard_before_json"])
                        if intent["shard_before_json"]
                        else None
                    )
                    with self.hub.transaction(shard) as session:
                        self.repository.restore_snapshot(
                            session, before, int(intent["order_id"])
                        )
                if legacy is not None:
                    before = (
                        json.loads(intent["legacy_before_json"])
                        if intent["legacy_before_json"]
                        else None
                    )
                    with self.hub.transaction("legacy") as session:
                        self.repository.restore_snapshot(
                            session, before, int(intent["order_id"])
                        )
                self.ledger.mark_dual_recovered(
                    write_id, "FAILED", "one side committed; compensated on restart"
                )
            except Exception as exc:
                LOG.error("dual-write recovery failed for %s: %s", write_id, exc)

    def start_recovery_threads(self) -> None:
        for run in self.ledger.pending_check_runs():
            run_id = str(run["run_id"])
            thread = threading.Thread(
                target=self._background_check,
                args=(run_id,),
                name=f"check-resume-{run_id[:8]}",
                daemon=True,
            )
            thread.start()
            self._recovery_threads.append(thread)
        for run in self.ledger.pending_rollback_runs():
            run_id = str(run["run_id"])
            try:
                self.gate.drain(self.config.rollback.wait_timeout_seconds)
            except AppError:
                LOG.error("cannot drain writes to resume rollback %s", run_id)
                continue
            thread = threading.Thread(
                target=self._background_rollback,
                args=(run_id,),
                name=f"rollback-resume-{run_id[:8]}",
                daemon=True,
            )
            thread.start()
            self._recovery_threads.append(thread)

    def _background_check(self, run_id: str) -> None:
        try:
            self.check(run_id=run_id)
        except Exception as exc:
            LOG.error("background check resume failed for %s: %s", run_id, exc)

    def _background_rollback(self, run_id: str) -> None:
        try:
            self.rollback(run_id=run_id)
        except Exception as exc:
            LOG.error("background rollback resume failed for %s: %s", run_id, exc)

    # -------------------------------------------------------------------- trace

    def trace(self, order_id: int) -> dict[str, Any]:
        return self.ledger.trace_order(order_id)
