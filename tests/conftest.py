"""Shared fixtures: isolated generated artifacts, tight timeouts and free ports."""

from __future__ import annotations

import socket
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from gateway import generator  # noqa: E402
from gateway.app import create_app  # noqa: E402
from gateway.config import GatewayConfig  # noqa: E402


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


VERSION_CFG = {
    "v3": {
        "max_queue_depth": 32,
        "max_concurrency": 1,
        "min_batch_size": 1,
        "max_batch_size": 4,
        "min_wait_ms": 1.0,
        "max_wait_ms": 5.0,
        "enqueue_timeout_ms": 400.0,
        "ready_wait_ms": 300.0,
        "circuit_p99_ms": 80.0,
        "circuit_error_rate": 0.5,
        "circuit_min_samples": 6,
        "window_seconds": 30.0,
    },
    "v4": {
        "max_queue_depth": 16,
        "max_concurrency": 1,
        "min_batch_size": 1,
        "max_batch_size": 4,
        "min_wait_ms": 1.0,
        "max_wait_ms": 5.0,
        "enqueue_timeout_ms": 1500.0,
        "ready_wait_ms": 300.0,
        "circuit_p99_ms": 80.0,
        "circuit_error_rate": 0.5,
        "circuit_min_samples": 6,
        "window_seconds": 30.0,
    },
}


@pytest.fixture(scope="session")
def generated_dir(tmp_path_factory) -> Path:
    out = tmp_path_factory.mktemp("artifacts")
    generator.generate(out)
    return out


@pytest.fixture
def config(generated_dir: Path) -> GatewayConfig:
    return GatewayConfig(
        host="127.0.0.1",
        port=_free_port(),
        metrics_port=_free_port(),
        model_dir=str(generated_dir / "models"),
        stable_version="v3",
        canary_version="v4",
        default_canary_weight=0.3,
        versions=VERSION_CFG,
    )


@pytest.fixture
def app(config):
    application = create_app(config)
    return application


@pytest.fixture
def service(app):
    return app.state.service


@pytest.fixture
def client(app):
    from fastapi.testclient import TestClient

    with TestClient(app) as test_client:
        yield test_client


@pytest.fixture(autouse=True)
def _reset_routing_and_breaker(service):
    """Keep routing weights and circuit breaker deterministic across tests."""

    service.router.set_default_weight(0.3)
    service.breaker.reset()
    yield
    service.router.set_default_weight(0.3)
    service.breaker.reset()
    for pipeline in service.pipelines.values():
        pipeline.inject_slow_calls(0, 0.0)
