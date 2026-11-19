from __future__ import annotations

import json
import signal
import threading
from dataclasses import dataclass
from typing import Any

from .config import ServiceConfig
from .db import Connector, SqlClient, pyodbc_connector
from .faults import ReplicationFaultController
from .http_api import Application, ServiceHttpServer
from .metrics import MetricsCollector
from .replication import ReplicaMonitor, SqlReplicationProbe
from .repository import SqlOrderGateway
from .service import OrderService
from .topology import TopologyValidator


@dataclass
class RunningApplication:
    config: ServiceConfig
    service: OrderService
    monitor: ReplicaMonitor
    faults: ReplicationFaultController
    httpd: ServiceHttpServer

    def start(self) -> None:
        self.monitor.collect_once()
        self.monitor.start()

    def serve_forever(self) -> None:
        stop = threading.Event()

        def request_stop(signum: int, frame: Any) -> None:
            stop.set()

        previous_handlers: dict[int, Any] = {}
        for signal_name in ("SIGINT", "SIGTERM"):
            signal_value = getattr(signal, signal_name, None)
            if signal_value is None:
                continue
            try:
                previous_handlers[signal_value] = signal.getsignal(signal_value)
                signal.signal(signal_value, request_stop)
            except (ValueError, OSError):
                pass

        server_thread = threading.Thread(
            target=self.httpd.serve_forever,
            name="http-server",
            daemon=True,
        )
        server_thread.start()
        try:
            while not stop.wait(1.0):
                pass
        except KeyboardInterrupt:
            pass
        finally:
            self.httpd.shutdown()
            server_thread.join(timeout=10)
            self.close()
            for signal_value, handler in previous_handlers.items():
                try:
                    signal.signal(signal_value, handler)
                except (ValueError, OSError):
                    pass

    def close(self) -> None:
        self.httpd.server_close()
        self.monitor.stop()
        self.service.close()


def build_application(
    config: ServiceConfig,
    connector: Connector = pyodbc_connector,
) -> RunningApplication:
    topology = TopologyValidator(config, connector).validate()

    primary_client = SqlClient(config.primary, connector)
    replica_client = SqlClient(config.replica, connector)
    primary_gateway = SqlOrderGateway(primary_client)
    replica_gateway = SqlOrderGateway(replica_client)
    probe = SqlReplicationProbe(
        config.primary, config.replica, config.replication, connector
    )
    monitor = ReplicaMonitor(probe, config.replication)
    metrics = MetricsCollector(monitor)
    service = OrderService(
        config,
        primary_gateway,
        replica_gateway,
        monitor,
        metrics,
        topology,
    )
    faults = ReplicationFaultController(service, monitor, connector)
    httpd = ServiceHttpServer(
        (config.server.host, config.server.port),
        Application(service=service, faults=faults),
        max_workers=config.server.request_threads,
    )
    return RunningApplication(
        config=config,
        service=service,
        monitor=monitor,
        faults=faults,
        httpd=httpd,
    )


def ready_payload(config: ServiceConfig) -> str:
    return json.dumps(
        {
            "event": "ready",
            "host": config.server.host,
            "port": config.server.port,
            "replication_mode": config.replication.mode,
            "consistency_levels": ["eventual", "session", "strong"],
        },
        sort_keys=True,
        separators=(",", ":"),
    )

