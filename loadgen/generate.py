from __future__ import annotations

import argparse
import json
import logging
import threading
import time
import urllib.parse
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from app.http_client import get_json, post_json


LOGGER = logging.getLogger("loadgen")


@dataclass(frozen=True)
class TenantLoad:
    tenant_id: str
    rate_per_sec: float
    duration_sec: float


def _load_scenario(path: str) -> tuple[str, list[TenantLoad], list[dict[str, Any]], dict[str, Any]]:
    with open(path, "r", encoding="utf-8") as handle:
        document = json.load(handle)
    if not isinstance(document, dict):
        raise ValueError("scenario root must be an object")
    scenario_id = document.get("scenario_id", "local-scenario")
    if not isinstance(scenario_id, str) or not scenario_id:
        raise ValueError("scenario_id must be a non-empty string")
    raw_tenants = document.get("tenants")
    if not isinstance(raw_tenants, list) or not raw_tenants:
        raise ValueError("scenario tenants must be a non-empty array")
    loads: list[TenantLoad] = []
    seen: set[str] = set()
    for item in raw_tenants:
        if not isinstance(item, dict):
            raise ValueError("scenario tenant entries must be objects")
        tenant_id = item.get("tenant_id")
        rate = item.get("rate_per_sec")
        duration = item.get("duration_sec")
        if not isinstance(tenant_id, str) or not tenant_id:
            raise ValueError("scenario tenant_id is required")
        if tenant_id in seen:
            raise ValueError(f"duplicate scenario tenant {tenant_id}")
        if not isinstance(rate, (int, float)) or isinstance(rate, bool) or rate <= 0:
            raise ValueError(f"tenant {tenant_id} rate_per_sec must be positive")
        if (
            not isinstance(duration, (int, float))
            or isinstance(duration, bool)
            or duration <= 0
        ):
            raise ValueError(f"tenant {tenant_id} duration_sec must be positive")
        seen.add(tenant_id)
        loads.append(TenantLoad(tenant_id, float(rate), float(duration)))

    raw_faults = document.get("faults", [])
    if not isinstance(raw_faults, list):
        raise ValueError("faults must be an array")
    faults: list[dict[str, Any]] = []
    for fault in raw_faults:
        if not isinstance(fault, dict):
            raise ValueError("fault entries must be objects")
        tenant_id = fault.get("tenant_id")
        at_sec = fault.get("at_sec")
        control = fault.get("control")
        if tenant_id not in seen:
            raise ValueError(f"fault tenant {tenant_id} is not in the load list")
        if (
            not isinstance(at_sec, (int, float))
            or isinstance(at_sec, bool)
            or at_sec < 0
        ):
            raise ValueError("fault at_sec must be non-negative")
        if not isinstance(control, dict):
            raise ValueError("fault control must be an object")
        faults.append(
            {
                "tenant_id": tenant_id,
                "at_sec": float(at_sec),
                "control": control,
            }
        )
    faults.sort(key=lambda item: (item["at_sec"], item["tenant_id"]))
    return scenario_id, loads, faults, document


class LoadGenerator:
    def __init__(self, coordinator: str, scenario_path: str):
        self.coordinator = coordinator.rstrip("/")
        (
            self.scenario_id,
            self.loads,
            self.faults,
            self.document,
        ) = _load_scenario(scenario_path)
        self.batch_interval = (
            float(self.document.get("batch_interval_ms", 100)) / 1000.0
        )
        self.drain_timeout = float(self.document.get("drain_timeout_sec", 45))
        self.status_poll = float(self.document.get("status_poll_sec", 0.5))
        self.failure: BaseException | None = None
        self.failure_lock = threading.Lock()
        self.run_id = uuid.uuid4().hex[:12]

    def _fail(self, exc: BaseException) -> None:
        with self.failure_lock:
            if self.failure is None:
                self.failure = exc

    def _control(self, tenant_id: str, control: dict[str, Any]) -> None:
        status, body, error = post_json(
            f"{self.coordinator}/v1/tenants/"
            f"{urllib.parse.quote(tenant_id, safe='')}/control",
            control,
            timeout_ms=3000,
        )
        if status is None or not 200 <= status < 300:
            raise RuntimeError(
                f"receiver control failed for {tenant_id}: "
                f"status={status} error={error} body={body}"
            )

    def _fault_schedule(self, start: float) -> None:
        try:
            for fault in self.faults:
                target = start + float(fault["at_sec"])
                remaining = target - time.monotonic()
                if remaining > 0:
                    time.sleep(remaining)
                self._control(fault["tenant_id"], fault["control"])
                LOGGER.info(
                    "fault applied tenant=%s at=%.1fs control=%s",
                    fault["tenant_id"],
                    fault["at_sec"],
                    fault["control"],
                )
        except BaseException as exc:
            self._fail(exc)

    def _send_batch(
        self, tenant: TenantLoad, first_sequence: int, count: int
    ) -> tuple[int, int]:
        events = []
        for sequence in range(first_sequence, first_sequence + count):
            events.append(
                {
                    "event_id": (
                        f"{self.scenario_id}-{self.run_id}-"
                        f"{tenant.tenant_id}-{sequence:08d}"
                    ),
                    "tenant_id": tenant.tenant_id,
                    "payload": {
                        "scenario_id": self.scenario_id,
                        "run_id": self.run_id,
                        "sequence": sequence,
                        "tenant_id": tenant.tenant_id,
                    },
                }
            )
        last_error = ""
        for attempt in range(4):
            status, body, error = post_json(
                f"{self.coordinator}/v1/events/batch",
                {"events": events},
                timeout_ms=10000,
            )
            if status is not None and 200 <= status < 300:
                return int((body or {}).get("inserted", 0)), int(
                    (body or {}).get("duplicates", 0)
                )
            last_error = f"status={status} error={error} body={body}"
            time.sleep(0.1 * (2**attempt))
        raise RuntimeError(
            f"event ingest failed for {tenant.tenant_id}: {last_error}"
        )

    def _generate_tenant(self, tenant: TenantLoad, start: float) -> None:
        try:
            total = max(0, int(round(tenant.rate_per_sec * tenant.duration_sec)))
            sent = 0
            while sent < total:
                now = time.monotonic()
                if now > start + tenant.duration_sec and sent >= total:
                    break
                due = min(total, int((now - start) * tenant.rate_per_sec))
                if due > sent:
                    count = min(256, due - sent)
                    inserted, duplicates = self._send_batch(tenant, sent, count)
                    sent += count
                    if inserted + duplicates != count:
                        LOGGER.warning(
                            "tenant %s batch returned inserted=%s duplicates=%s "
                            "for count=%s",
                            tenant.tenant_id,
                            inserted,
                            duplicates,
                            count,
                        )
                    continue
                remaining = max(0.001, self.batch_interval)
                time.sleep(min(remaining, 0.05))
            if sent < total:
                self._send_batch(tenant, sent, total - sent)
                sent = total
            LOGGER.info("generated %s events for %s", sent, tenant.tenant_id)
        except BaseException as exc:
            self._fail(exc)

    def _pending(self) -> int:
        status, body, error = get_json(
            f"{self.coordinator}/v1/status", timeout_ms=3000
        )
        if status != 200 or not isinstance(body, dict):
            raise RuntimeError(f"status query failed: status={status} error={error}")
        requested = {load.tenant_id for load in self.loads}
        pending = 0
        for tenant in body.get("tenants", []):
            if tenant.get("tenant_id") in requested:
                pending += int(tenant.get("queue_depth", 0))
                pending += int(tenant.get("inflight", 0))
        return pending

    def run(self) -> dict[str, Any]:
        status, _, error = get_json(
            f"{self.coordinator}/v1/status", timeout_ms=3000
        )
        if status != 200:
            raise RuntimeError(f"coordinator is not ready: {status} {error}")

        for fault in [item for item in self.faults if item["at_sec"] == 0]:
            self._control(fault["tenant_id"], fault["control"])

        start = time.monotonic() + 0.2
        later_faults = [item for item in self.faults if item["at_sec"] > 0]
        self.faults = later_faults
        schedule_thread = threading.Thread(
            target=self._fault_schedule,
            args=(start,),
            name="scenario-faults",
            daemon=True,
        )
        schedule_thread.start()
        generators = [
            threading.Thread(
                target=self._generate_tenant,
                args=(load, start),
                name=f"loadgen-{load.tenant_id}",
                daemon=True,
            )
            for load in self.loads
        ]
        for thread in generators:
            thread.start()
        for thread in generators:
            thread.join()
        schedule_thread.join()
        if self.failure is not None:
            raise self.failure

        deadline = time.monotonic() + self.drain_timeout
        while time.monotonic() < deadline:
            if self._pending() == 0:
                break
            time.sleep(max(0.05, self.status_poll))
        else:
            LOGGER.warning("drain timeout reached; report may contain queued events")

        status, report, error = get_json(
            f"{self.coordinator}/v1/report", timeout_ms=10000
        )
        if status != 200 or not isinstance(report, dict):
            raise RuntimeError(f"report query failed: status={status} error={error}")
        report["scenario_id"] = self.scenario_id
        return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Webhook lab fixed-rate load generator")
    parser.add_argument("--scenario", required=True)
    parser.add_argument(
        "--coordinator", default="http://127.0.0.1:18080"
    )
    parser.add_argument("--output")
    args = parser.parse_args(argv)
    logging.basicConfig(
        level="INFO", format="%(asctime)s %(levelname)s %(name)s %(message)s"
    )
    try:
        report = LoadGenerator(args.coordinator, args.scenario).run()
    except Exception as exc:
        LOGGER.error("scenario failed: %s", exc)
        return 1
    rendered = json.dumps(report, indent=2, sort_keys=True)
    if args.output:
        Path(args.output).write_text(rendered + "\n", encoding="utf-8")
    print(rendered)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

