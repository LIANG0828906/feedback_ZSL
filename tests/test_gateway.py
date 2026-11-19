from __future__ import annotations

import json
import time
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from gateway.app import create_app
from gateway.core import ModelRegistry

ROOT = Path(__file__).resolve().parents[1]
CONFIG_PATH = ROOT / "config.json"

V1 = [
    "user_age",
    "item_age",
    "ctr_7d",
    "cvr_7d",
    "price",
    "category_affinity",
    "freshness",
    "query_match",
]
V2 = [
    "user_age",
    "item_age",
    "price",
    "category_affinity",
    "ctr_7d",
    "cvr_7d",
    "discount",
    "delivery_eta",
    "freshness",
    "query_match",
]


def _wait(client: TestClient, predicate, timeout: float = 5.0) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        state = client.get("/state").json()
        if predicate(state):
            return state
        time.sleep(0.02)
    raise AssertionError("gateway state condition was not met before timeout")


def _predict(
    client: TestClient,
    *,
    tenant: str,
    client_id: str | None,
    protocol: str,
    names: list[str],
    values: list[float] | None = None,
):
    payload = {
        "tenant_id": tenant,
        "protocol_version": protocol,
        "feature_names": names,
        "features": values if values is not None else [0.25] * len(names),
    }
    if client_id is not None:
        payload["client_id"] = client_id
    return client.post("/predict", json=payload)


def test_dual_versions_hot_weights_and_protocol_errors() -> None:
    app = create_app(CONFIG_PATH)
    with TestClient(app) as client:
        state = _wait(
            client,
            lambda item: all(
                version["load_state"] == "READY"
                for version in item["versions"].values()
            ),
        )
        assert state["versions"]["v3"]["protocol_version"] == "ranking-features-v1"
        assert state["versions"]["v4"]["protocol_version"] == "ranking-features-v2"
        assert state["versions"]["v4"]["input_shape"] == [1, 10]

        stable_weights = client.post(
            "/admin/weights",
            json={"tenant_id": "tenant-standard", "weights": {"v3": 1.0, "v4": 0.0}},
        )
        assert stable_weights.status_code == 200
        stable = _predict(
            client,
            tenant="tenant-standard",
            client_id="stable-client",
            protocol="ranking-features-v1",
            names=V1,
        )
        assert stable.status_code == 200
        assert stable.json()["model_version"] == "v3"

        canary = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v2",
            names=V2,
        )
        assert canary.status_code == 200
        assert canary.json()["model_version"] == "v4"

        for target, protocol, names in (
            ("v3", "ranking-features-v1", V1),
            ("v4", "ranking-features-v2", V2),
            ("v3", "ranking-features-v1", V1),
        ):
            weights = (
                {"v3": 1.0, "v4": 0.0}
                if target == "v3"
                else {"v3": 0.0, "v4": 1.0}
            )
            client.post(
                "/admin/weights",
                json={"tenant_id": "tenant-switch", "weights": weights},
            )
            response = _predict(
                client,
                tenant="tenant-switch",
                client_id="same-client",
                protocol=protocol,
                names=names,
            )
            assert response.status_code == 200
            assert response.json()["model_version"] == target

        mismatch = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v1",
            names=V1,
        )
        assert mismatch.status_code == 422
        assert mismatch.json()["error"]["code"] == "MODEL_PROTOCOL_MISMATCH"
        assert mismatch.json()["error"]["field"] == "protocol_version"
        assert "score" not in mismatch.json()

        swapped = list(V2)
        swapped[2], swapped[4] = swapped[4], swapped[2]
        order_error = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v2",
            names=swapped,
        )
        assert order_error.status_code == 422
        assert order_error.json()["error"]["code"] == "FEATURE_ORDER_MISMATCH"
        assert order_error.json()["error"]["field"] == "feature_names[2]"

        count_error = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v2",
            names=V2[:-1],
            values=[0.2] * 9,
        )
        assert count_error.status_code == 422
        assert count_error.json()["error"]["code"] == "FEATURE_COUNT_MISMATCH"
        assert count_error.json()["error"]["field"] == "features"

        metrics = client.get("/metrics")
        assert metrics.status_code == 200
        assert "ranking_gateway_version_requests_total" in metrics.text
        assert "ranking_gateway_version_weight" in metrics.text
        assert 'ranking_gateway_version_weight{version="v4"}' in metrics.text


def test_latency_injection_triggers_rollback_and_preserves_stable_traffic() -> None:
    app = create_app(CONFIG_PATH)
    with TestClient(app) as client:
        _wait(
            client,
            lambda item: all(
                version["ready"] for version in item["versions"].values()
            ),
        )
        client.post(
            "/admin/weights",
            json={"tenant_id": "tenant-canary", "weights": {"v3": 0.0, "v4": 1.0}},
        )
        client.post(
            "/admin/weights",
            json={"tenant_id": "tenant-standard", "weights": {"v3": 1.0, "v4": 0.0}},
        )
        for _index in range(8):
            response = _predict(
                client,
                tenant="tenant-canary",
                client_id="canary-client",
                protocol="ranking-features-v2",
                names=V2,
            )
            assert response.status_code == 200

        fault = client.post(
            "/admin/faults", json={"inference_delay_ms": {"v4": 50}}
        )
        assert fault.status_code == 200
        delayed = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v2",
            names=V2,
        )
        assert delayed.status_code == 200
        assert delayed.json()["latency_ms"] >= 45

        state = _wait(
            client,
            lambda item: item["rollback"]["count"] >= 1,
            timeout=5.0,
        )
        assert state["rollback"]["active_versions"] == ["v4"]
        assert state["routing"]["weights"]["v4"] == 0.0
        assert state["rollback"]["recent"][-1]["trigger_metric"] in {
            "p99_latency_ms",
            "error_rate",
        }

        stable = _predict(
            client,
            tenant="tenant-standard",
            client_id="stable-client",
            protocol="ranking-features-v1",
            names=V1,
        )
        assert stable.status_code == 200
        assert stable.json()["model_version"] == "v3"
        metrics = client.get("/metrics").text
        assert 'ranking_gateway_version_weight{version="v4"} 0' in metrics
        assert "ranking_gateway_rollbacks_total 1" in metrics


def test_reload_failure_keeps_previous_ready_session() -> None:
    app = create_app(CONFIG_PATH)
    with TestClient(app) as client:
        _wait(
            client,
            lambda item: all(
                version["ready"] for version in item["versions"].values()
            ),
        )
        client.post(
            "/admin/weights",
            json={"tenant_id": "tenant-standard", "weights": {"v3": 1.0, "v4": 0.0}},
        )
        before = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v2",
            names=V2,
        )
        assert before.status_code == 200
        assert before.json()["model_version"] == "v4"

        client.post("/admin/faults", json={"fail_next_load": ["v4"]})
        reload_response = client.post("/admin/reload/v4?wait=true")
        assert reload_response.status_code == 409
        assert reload_response.json()["error_code"] == "MODEL_LOAD_FAILED"

        state = client.get("/state").json()
        assert state["versions"]["v4"]["ready"] is True
        assert state["versions"]["v4"]["load_state"] == "READY"
        assert (
            state["versions"]["v4"]["last_load_error_code"] == "MODEL_LOAD_FAILED"
        )
        stable = _predict(
            client,
            tenant="tenant-standard",
            client_id="stable-client",
            protocol="ranking-features-v1",
            names=V1,
        )
        assert stable.status_code == 200
        assert stable.status_code != 503


def test_startup_load_failure_is_isolated(tmp_path: Path) -> None:
    config = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    config["fault_injection"]["startup_fail_versions"] = ["v4"]
    for model in config["models"].values():
        model["path"] = str((ROOT / model["path"]).resolve())
    config_path = tmp_path / "config.json"
    config_path.write_text(json.dumps(config), encoding="utf-8")

    app = create_app(config_path)
    with TestClient(app) as client:
        state = _wait(
            client,
            lambda item: item["versions"]["v3"]["load_state"] == "READY"
            and item["versions"]["v4"]["load_state"] == "FAILED",
        )
        assert state["versions"]["v4"]["last_load_error_code"] == "MODEL_LOAD_FAILED"
        stable = _predict(
            client,
            tenant="tenant-standard",
            client_id="stable-client",
            protocol="ranking-features-v1",
            names=V1,
        )
        assert stable.status_code == 200
        unavailable = _predict(
            client,
            tenant="tenant-canary",
            client_id="canary-client",
            protocol="ranking-features-v2",
            names=V2,
        )
        assert unavailable.status_code != 503
        assert unavailable.status_code == 422
        assert unavailable.json()["error"]["code"] == "MODEL_PROTOCOL_MISMATCH"


def test_request_waits_for_loading_model_without_503(monkeypatch) -> None:
    original = ModelRegistry._load_model

    def slow_load(self, slot):
        time.sleep(0.25)
        return original(self, slot)

    monkeypatch.setattr(ModelRegistry, "_load_model", slow_load)
    app = create_app(CONFIG_PATH)
    with TestClient(app) as client:
        response = _predict(
            client,
            tenant="tenant-canary",
            client_id="loading-client",
            protocol="ranking-features-v2",
            names=V2,
        )
        assert response.status_code == 200
        assert response.status_code != 503
        assert response.json()["model_version"] == "v4"
        state = client.get("/state").json()
        transitions = state["versions"]["v4"]["transitions"]
        assert any(item["to"] == "LOADING" for item in transitions)
        assert any(item["to"] == "READY" for item in transitions)
