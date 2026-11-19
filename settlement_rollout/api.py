from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlparse

from .config import PlanConfig
from .errors import RolloutError
from .orchestrator import RolloutOrchestrator
from .store import AuditStore


def _json_bytes(value: dict[str, Any]) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _label(value: Any) -> str:
    return str(value).replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def _number(value: Any, default: float = 0.0) -> str:
    try:
        numeric = float(value)
    except (TypeError, ValueError):
        numeric = default
    if not numeric == numeric or numeric in (float("inf"), float("-inf")):
        numeric = default
    return f"{numeric:.6f}".rstrip("0").rstrip(".")


def render_prometheus(
    store: AuditStore,
    config: PlanConfig,
    run_id: str,
) -> str:
    run = store.get_run(run_id)
    batches = store.get_batches(run_id)
    instances = store.get_instances(run_id)
    lines = [
        "# HELP settlement_rollout_run_info Current rollout run metadata.",
        "# TYPE settlement_rollout_run_info gauge",
        (
            'settlement_rollout_run_info{run_id="%s",release_id="%s",status="%s"} 1'
            % (_label(run_id), _label(run["release_id"]), _label(run["status"]))
        ),
        "# HELP settlement_rollout_batch_instance_version_info Active version by batch instance.",
        "# TYPE settlement_rollout_batch_instance_version_info gauge",
    ]
    batch_entries: list[tuple[dict[str, Any], dict[str, Any]]] = []
    for batch in batches:
        snapshot = store.latest_snapshot(run_id, batch["batch_id"])
        batch_entries.append((batch, snapshot))
        batch_config = next(
            item for item in config.batches if item.batch_id == batch["batch_id"]
        )
        for instance_id in batch_config.instances:
            active = instances.get(instance_id, {})
            version = active.get("active_version", "unknown")
            lines.append(
                (
                    'settlement_rollout_batch_instance_version_info{run_id="%s",'
                    'batch_id="%s",instance_id="%s",version="%s"} 1'
                )
                % (
                    _label(run_id),
                    _label(batch["batch_id"]),
                    _label(instance_id),
                    _label(version),
                )
            )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_switched_instances Instances currently on the release target version.",
            "# TYPE settlement_rollout_batch_switched_instances gauge",
        ]
    )
    for batch, _ in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}"'
        )
        lines.append(
            f"settlement_rollout_batch_switched_instances{{{labels}}} "
            f"{len(batch['switched_instances'])}"
        )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_sli_availability_percent Current batch availability SLI.",
            "# TYPE settlement_rollout_batch_sli_availability_percent gauge",
        ]
    )
    for batch, snapshot in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}"'
        )
        lines.append(
            f"settlement_rollout_batch_sli_availability_percent{{{labels}}} "
            f"{_number(snapshot.get('availability_percent'))}"
        )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_sli_p99_latency_ms Current batch P99 latency SLI.",
            "# TYPE settlement_rollout_batch_sli_p99_latency_ms gauge",
        ]
    )
    for batch, snapshot in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}"'
        )
        lines.append(
            f"settlement_rollout_batch_sli_p99_latency_ms{{{labels}}} "
            f"{_number(snapshot.get('p99_latency_ms'))}"
        )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_error_budget_remaining_ratio Remaining combined error budget ratio.",
            "# TYPE settlement_rollout_batch_error_budget_remaining_ratio gauge",
        ]
    )
    for batch, snapshot in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}"'
        )
        lines.append(
            f"settlement_rollout_batch_error_budget_remaining_ratio{{{labels}}} "
            f"{_number(snapshot.get('error_budget_remaining_ratio'), 1.0)}"
        )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_decisions_total Number of final decisions recorded for a batch.",
            "# TYPE settlement_rollout_batch_decisions_total counter",
        ]
    )
    for batch, _ in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}"'
        )
        lines.append(
            f"settlement_rollout_batch_decisions_total{{{labels}}} "
            f"{batch['decision_count']}"
        )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_rollbacks_total Number of per-instance rollback actions.",
            "# TYPE settlement_rollout_batch_rollbacks_total counter",
        ]
    )
    for batch, _ in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}"'
        )
        lines.append(
            f"settlement_rollout_batch_rollbacks_total{{{labels}}} "
            f"{batch['rollback_count']}"
        )
    lines.extend(
        [
            "# HELP settlement_rollout_batch_status_info Current batch state.",
            "# TYPE settlement_rollout_batch_status_info gauge",
        ]
    )
    for batch, _ in batch_entries:
        labels = (
            f'run_id="{_label(run_id)}",batch_id="{_label(batch["batch_id"])}",'
            f'status="{_label(batch["status"])}"'
        )
        lines.append(f"settlement_rollout_batch_status_info{{{labels}}} 1")
    return "\n".join(lines) + "\n"


class OrchestratorAPI:
    def __init__(
        self,
        controller: RolloutOrchestrator,
        store: AuditStore,
        config: PlanConfig,
        host: str,
        port: int,
    ) -> None:
        self.controller = controller
        self.store = store
        self.config = config
        self.server = APIServer((host, port), self)
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        self._thread = threading.Thread(
            target=self.server.serve_forever,
            kwargs={"poll_interval": 0.1},
            daemon=True,
        )
        self._thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        if self._thread is not None:
            self._thread.join(timeout=2.0)


class APIHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "SettlementRolloutAPI/1.0"

    @property
    def api(self) -> OrchestratorAPI:
        return self.server.api  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: Any) -> None:
        return

    def _send(
        self,
        status: int,
        body: bytes,
        content_type: str = "application/json; charset=utf-8",
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, status: int, value: dict[str, Any]) -> None:
        self._send(status, _json_bytes(value))

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path == "/health":
            self._send_json(200, {"ok": True})
            return
        if parsed.path == "/status":
            self._send_json(200, self.api.controller.status())
            return
        if parsed.path == "/metrics":
            text = render_prometheus(
                self.api.store,
                self.api.config,
                self.api.controller.run_id,
            )
            self._send(
                200,
                text.encode("utf-8"),
                "text/plain; version=0.0.4; charset=utf-8",
            )
            return
        if parsed.path == "/audit":
            query = parse_qs(parsed.query)
            batch_id = query.get("batch_id", [None])[0]
            if batch_id:
                try:
                    audit = self.api.store.audit_batch(
                        self.api.controller.run_id, batch_id
                    )
                except RolloutError as exc:
                    self._send_json(404, exc.as_dict())
                    return
                self._send_json(
                    200,
                    {
                        "ok": True,
                        "run_id": self.api.controller.run_id,
                        "audit": audit,
                    },
                )
                return
            audits = [
                self.api.store.audit_batch(
                    self.api.controller.run_id, batch["batch_id"]
                )
                for batch in self.api.store.get_batches(
                    self.api.controller.run_id
                )
            ]
            self._send_json(
                200,
                {
                    "ok": True,
                    "run_id": self.api.controller.run_id,
                    "audits": audits,
                },
            )
            return
        self._send_json(404, {"ok": False, "code": "NOT_FOUND"})

    def do_POST(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path != "/abort":
            self._send_json(404, {"ok": False, "code": "NOT_FOUND"})
            return
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(raw.decode("utf-8")) if raw else {}
        except json.JSONDecodeError:
            self._send_json(400, {"ok": False, "code": "INVALID_JSON"})
            return
        reason = payload.get("reason", "manual_abort")
        try:
            result = self.api.controller.abort(str(reason))
        except RolloutError as exc:
            self._send_json(409, exc.as_dict())
            return
        self._send_json(200, result)


class APIServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 64

    def __init__(self, address: tuple[str, int], api: OrchestratorAPI) -> None:
        super().__init__(address, APIHandler)
        self.api = api
