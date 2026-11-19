"""Prometheus 文本格式指标暴露。

稳定指标名（见 README 指标表），instance 标签取值固定 primary/replica。
"""
from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from config import Config
from slots import collect
from store import Store


def render_metrics(cfg: Config, store: Store) -> str:
    view = collect(cfg)
    lines: list[str] = []

    def gauge(name: str, help_text: str, value: float,
              labels: dict[str, str] | None = None) -> None:
        label_str = (
            "{" + ",".join(f'{k}="{v}"' for k, v in labels.items()) + "}"
            if labels
            else ""
        )
        lines.append(f"{name}{label_str} {int(value)}")

    header_emitted: set[str] = set()

    def header(name: str, help_text: str, mtype: str = "gauge") -> None:
        if name in header_emitted:
            return
        lines.append(f"# HELP {name} {help_text}")
        lines.append(f"# TYPE {name} {mtype}")
        header_emitted.add(name)

    header("slotguard_replication_slots",
           "Number of replication slots visible on the instance")
    header("slotguard_inactive_replication_slots",
           "Number of inactive replication slots on the instance")
    header("slotguard_slot_lag_bytes_max",
           "Maximum confirmed-LSN lag in bytes for the instance")
    header("slotguard_retained_wal_bytes_max",
           "Maximum WAL bytes retained by inactive slots")

    for instance in cfg.instances:
        payload = view["per_instance"][instance]
        slots_ = payload["slots"]
        inactive = [s for s in slots_ if not s["active"]]
        max_lag = max((s["lag_bytes"] or 0 for s in slots_), default=0)
        max_retained = max(
            (s["retained_wal_bytes"] or 0 for s in inactive), default=0
        )
        labels = {"instance": instance}
        gauge("slotguard_replication_slots", "", len(slots_), labels)
        gauge("slotguard_inactive_replication_slots", "", len(inactive), labels)
        gauge("slotguard_slot_lag_bytes_max", "", max_lag, labels)
        gauge("slotguard_retained_wal_bytes_max", "", max_retained, labels)

    header("slotguard_resync_tables_in_progress",
           "Tables currently in resync running state")
    in_progress = store.count_resync_tables_in_progress()
    gauge("slotguard_resync_tables_in_progress", "", in_progress)

    header("slotguard_offset_corrections_total",
           "Cumulative Redis offset corrections", "counter")
    gauge("slotguard_offset_corrections_total", "",
          store.get_counter("offset_corrections_total"))

    header("slotguard_sample_mismatches_total",
           "Cumulative sampling mismatches", "counter")
    gauge("slotguard_sample_mismatches_total", "",
          store.get_counter("sample_mismatches_total"))

    header("slotguard_query_errors_total",
           "Cumulative instance query errors", "counter")
    gauge("slotguard_query_errors_total", "", store.count_query_errors())
    return "\n".join(lines) + "\n"


class MetricsHandler(BaseHTTPRequestHandler):
    cfg: Config
    store: Store

    def log_message(self, *args):  # 静默访问日志
        return

    def do_GET(self):  # noqa: N802
        if self.path not in ("/metrics", "/"):
            self.send_response(404)
            self.end_headers()
            return
        body = render_metrics(self.cfg, self.store).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type",
                         "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def start_metrics_server(cfg: Config, store: Store) -> ThreadingHTTPServer:
    handler = type(
        "BoundMetricsHandler",
        (MetricsHandler,),
        {"cfg": cfg, "store": store},
    )
    server = ThreadingHTTPServer((cfg.metrics_host, cfg.metrics_port), handler)
    thread = threading.Thread(target=server.serve_forever, name="metrics-http",
                              daemon=True)
    thread.start()
    return server
