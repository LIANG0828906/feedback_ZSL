"""Periodic monitoring, sampling, offset reconciliation and metrics."""
from __future__ import annotations

import logging
import time
import uuid
from typing import Any

import relief
import slots
from config import Config
from offsets import OffsetReconciler
from resync import ResyncManager
from sampling import Sampler
from store import Store

log = logging.getLogger("slotguard")


def _round_id(prefix: str) -> str:
    stamp = time.strftime("%Y%m%dT%H%M%S", time.gmtime())
    return f"{prefix}-{stamp}-{uuid.uuid4().hex[:8]}"


class SlotGuardService:
    def __init__(self, cfg: Config, store: Store):
        self.cfg = cfg
        self.store = store
        self.resync = ResyncManager(cfg, store)
        self.sampler = Sampler(cfg, store)
        self.reconciler = OffsetReconciler(cfg, store)

    def close(self) -> None:
        self.reconciler.close()

    def monitor_once(self, round_id: str | None = None) -> dict[str, Any]:
        round_id = round_id or _round_id("mon")
        now = time.time()
        if not self.store.begin_round(round_id, "monitor", now):
            return {"round_id": round_id, "skipped": "round already completed"}
        try:
            view = slots.collect(self.cfg)
            errors: list[dict[str, Any]] = []
            for instance, payload in view["per_instance"].items():
                for error in payload.get("errors", []):
                    errors.append(error)
                    self.store.insert_query_error(
                        round_id, error["instance"], error["target"],
                        error["code"], error["message"], now,
                    )
                for slot in payload["slots"]:
                    self.store.insert_slot_snapshot(
                        {
                            "round_id": round_id,
                            "instance": instance,
                            "slot_name": slot["slot_name"],
                            "plugin": slot["plugin"],
                            "slot_type": slot["slot_type"],
                            "active": int(slot["active"]),
                            "restart_lsn": slot["restart_lsn"],
                            "confirmed_lsn": slot["confirmed_lsn"],
                            "current_lsn": slot["current_lsn"],
                            "lag_bytes": slot["lag_bytes"],
                            "wal_retention": slot["retained_wal_bytes"],
                            "pid": slot["active_pid"],
                            "subscription": slot["subscription_name"],
                            "inactive_since": slot["inactive_since"],
                            "inactive_seconds": slot["inactive_seconds"],
                        }
                    )

            advices = relief.evaluate(
                self.cfg, self.store, round_id, view, now
            )
            self.store.finish_round(
                round_id, "done", time.time(),
                detail=f"{len(errors)} query error(s)",
            )
            return {
                "round_id": round_id,
                "slots": view,
                "advices": advices,
                "errors": errors,
            }
        except Exception:
            self.store.rollback()
            raise

    def sampling_once(self, round_id: str | None = None) -> dict[str, Any]:
        round_id = round_id or _round_id("smp")
        if not self.store.begin_round(round_id, "sampling", time.time()):
            return {"round_id": round_id, "skipped": "round already completed"}
        try:
            result = self.sampler.run_round(round_id)
            self.store.finish_round(round_id, "done", time.time())
            return result
        except Exception:
            self.store.rollback()
            raise

    def reconcile_once(self, round_id: str | None = None) -> dict[str, Any]:
        round_id = round_id or _round_id("off")
        if not self.store.begin_round(round_id, "offset", time.time()):
            return {"round_id": round_id, "skipped": "round already completed"}
        try:
            result = self.reconciler.reconcile(round_id)
            self.store.finish_round(round_id, "done", time.time())
            return result
        except Exception:
            self.store.rollback()
            raise

    def recover_incomplete_rounds(self) -> list[dict[str, Any]]:
        """Resume rounds whose durable marker survived a process crash."""
        recovered: list[dict[str, Any]] = []
        for row in self.store.list_incomplete_rounds():
            round_id, kind = row["round_id"], row["kind"]
            try:
                if kind == "monitor":
                    result = self.monitor_once(round_id)
                elif kind == "sampling":
                    result = self.sampling_once(round_id)
                elif kind == "offset":
                    result = self.reconcile_once(round_id)
                else:
                    continue
                recovered.append({"round_id": round_id, "kind": kind})
                log.info("recovered incomplete %s round %s", kind, round_id)
            except Exception:
                log.exception("failed to recover %s round %s", kind, round_id)
        return recovered

    def run_forever(self) -> None:
        self.recover_incomplete_rounds()
        if self.cfg.auto_resume_resync and self.resync.active_job():
            try:
                log.info("resuming unfinished resync job on startup")
                self.resync.run("auto-resume")
            except Exception:
                log.exception("automatic resync resume failed")
        last_sampling = 0.0
        last_reconcile = 0.0
        last_auto_resync = 0.0
        while True:
            try:
                cycle_start = time.time()
                monitor = self.monitor_once()
                if (
                    self.cfg.auto_rebootstrap
                    and time.time() - last_auto_resync
                    >= self.cfg.resync_retry_interval_s
                    and self._configured_slot_missing(monitor)
                ):
                    last_auto_resync = time.time()
                    try:
                        log.warning(
                            "configured slot %s is missing; starting automatic "
                            "rebootstrap",
                            self.cfg.slot_name,
                        )
                        self.resync.run("auto-slot-missing")
                    except Exception:
                        log.exception("automatic resync failed")
                if cycle_start - last_sampling >= self.cfg.sampling_interval_s:
                    try:
                        self.sampling_once()
                    except Exception:
                        log.exception("sampling round failed")
                    finally:
                        last_sampling = time.time()
                if cycle_start - last_reconcile >= self.cfg.reconcile_interval_s:
                    try:
                        self.reconcile_once()
                    except Exception:
                        log.exception("offset reconcile round failed")
                    finally:
                        last_reconcile = time.time()
                elapsed = time.time() - cycle_start
                sleep_s = max(1.0, self.cfg.monitor_interval_s - elapsed)
                time.sleep(sleep_s)
            except Exception:
                log.exception("monitor cycle failed; continuing next cycle")
                time.sleep(min(5, max(1, self.cfg.monitor_interval_s)))

    def _configured_slot_missing(self, monitor: dict[str, Any]) -> bool:
        per_instance = monitor.get("slots", {}).get("per_instance", {})
        primary = per_instance.get("primary")
        replica = per_instance.get("replica")
        if not primary or not replica or primary.get("errors") or replica.get("errors"):
            return False
        primary_slots = {
            slot["slot_name"] for slot in primary.get("slots", [])
        }
        return self.cfg.slot_name not in primary_slots
