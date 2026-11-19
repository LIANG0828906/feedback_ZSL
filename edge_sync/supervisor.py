"""Process-level lifecycle for target service, egress proxies and API."""

from __future__ import annotations

import time
from dataclasses import dataclass

from .config import Config
from .orchestrator import Orchestrator, ThreadingServiceServer, start_service
from .proxy import EgressProxyGroup
from .store import SyncStore
from .target_service import TargetStore, ThreadingTargetServer, start_target


@dataclass
class LocalStack:
    config: Config
    target_store: TargetStore
    target_server: ThreadingTargetServer
    proxy_group: EgressProxyGroup
    store: SyncStore
    orchestrator: Orchestrator
    service_server: ThreadingServiceServer

    def addresses(self) -> dict:
        return {
            "target": (self.config.target_host, self.config.target_port),
            "service": (self.config.service_host, self.config.service_port),
            "egresses": {
                egress.egress_id: (egress.listen_host, egress.listen_port)
                for egress in self.config.egresses
            },
        }

    @property
    def proxy_states(self) -> dict:
        return self.proxy_group.states()

    def shutdown(self) -> None:
        self.service_server.shutdown()
        self.service_server.server_close()
        self.proxy_group.stop_all()
        self.target_server.shutdown()
        self.target_server.server_close()
        self.store.close()
        self.target_store.close()


def bring_up_stack(config: Config, reset: bool = True) -> LocalStack:
    target_server, _, target_store = start_target(
        config.target_host, config.target_port, config.target_db_path
    )
    try:
        target_address = (config.target_host, config.target_port)
        proxy_group = EgressProxyGroup(
            config.egresses, config.seed, config.injections, target_address
        )
        proxy_group.start_all()
        store = SyncStore(config.db_path)
        if reset:
            store.reset()
        proxy_addresses = {
            egress.egress_id: (egress.listen_host, egress.listen_port) for egress in config.egresses
        }
        orchestrator = Orchestrator(
            config, store, proxy_group.states(), proxy_addresses, target_address
        )
        service_server, _ = start_service(config, orchestrator)
        _wait_for_port(config.service_host, config.service_port)
    except Exception:
        target_server.shutdown()
        target_server.server_close()
        target_store.close()
        raise
    return LocalStack(
        config=config,
        target_store=target_store,
        target_server=target_server,
        proxy_group=proxy_group,
        store=store,
        orchestrator=orchestrator,
        service_server=service_server,
    )


def _wait_for_port(host: str, port: int, timeout_s: float = 5.0) -> None:
    import socket

    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((host, port), timeout=0.5):
                return
        except OSError:
            time.sleep(0.05)
    raise TimeoutError(f"service on {host}:{port} did not start within {timeout_s}s")
