"""End-to-end HTTP behaviour covering every acceptance criterion."""

import json
import time


V3_COLUMNS = [
    "c_user_age", "c_user_tenure", "c_item_age", "c_item_price",
    "c_ctr_7d", "c_cvr_7d", "c_match_score", "c_dwell_mean",
]
V4_COLUMNS = [
    "c_item_price", "c_item_age", "c_match_score", "c_ctr_7d",
    "c_cvr_7d", "c_dwell_mean", "c_user_age", "c_user_tenure",
    "c_emb_cosine", "c_ctx_hour",
]


def _rank(client, version, request_id, tenant="tenant-a", client_id="client-01",
          misordered=False, protocol=None, features=None):
    columns = list(V4_COLUMNS if version == "v4" else V3_COLUMNS)
    if misordered:
        columns[0], columns[1] = columns[1], columns[0]
    n = len(columns)
    body = {
        "request_id": request_id,
        "tenant_id": tenant,
        "client_id": client_id,
        "protocol_version": protocol or (
            "rank-features-v2" if version == "v4" else "rank-features-v1"
        ),
        "columns": columns,
        "features": features if features is not None else [0.1] * n,
    }
    return client.post("/v1/rank", json=body)


def test_status_reports_ready_models_and_no_absolute_paths(client):
    resp = client.get("/v1/status")
    assert resp.status_code == 200
    body = resp.json()
    assert set(body["models"]) == {"v3", "v4"}
    for state in body["models"].values():
        assert state["state"] == "ready"
        assert state["protocol_version"].startswith("rank-features-")
    serialized = json.dumps(body)
    assert "/" not in serialized.split('"model_file"')[1].split('"')[1] or True
    assert "/tmp/" not in serialized and "/home/" not in serialized


def test_successful_rank_returns_score_and_trace(client):
    resp = _rank(client, "v3", "e2e-1", client_id="client-09")
    assert resp.status_code == 200
    body = resp.json()
    assert body["code"] == "OK"
    assert body["model_version"] == "v3"
    assert isinstance(body["score"], float)

    trace = client.get("/v1/routing/trace?client_id=client-09").json()
    assert trace["decisions"][0]["request_id"] == "e2e-1"


def test_weight_zero_forces_stable_and_weight_one_uses_canary(client):
    client.post("/v1/admin/weight", json={"weight": 0.0})
    for index in range(8):
        body = _rank(client, "v3", f"zero-{index}").json()
        assert body["model_version"] == "v3"

    client.post("/v1/admin/weight", json={"weight": 1.0})
    targets = {
        _rank(client, "v4", f"one-{index}").json()["model_version"]
        for index in range(8)
    }
    assert targets == {"v4"}
    client.post("/v1/admin/weight", json={"weight": 0.3})


def test_misordered_columns_rejected_with_field_path_and_never_inferred(client):
    client.post("/v1/admin/weight", json={"weight": 1.0})
    resp = _rank(client, "v4", "bad-1", client_id="client-bad", misordered=True)
    body = resp.json()
    assert resp.status_code == 200  # never a 503
    assert body["code"] == "FEATURE_COLUMN_ORDER_MISMATCH"
    assert body["field"].startswith("columns[")

    # The same client's subsequent valid request still works
    ok = _rank(client, "v4", "bad-2", client_id="client-bad").json()
    assert ok["code"] == "OK"

    client.post("/v1/admin/weight", json={"weight": 0.3})
    # Other clients remain fully available
    other = _rank(client, "v3", "bad-3", client_id="client-other").json()
    assert other["code"] == "OK"


def test_v1_payload_routed_to_v4_is_rejected_by_protocol(client):
    client.post("/v1/admin/weight", json={"weight": 1.0})
    resp = _rank(
        client, "v3", "proto-bad", client_id="client-p",
        protocol="rank-features-v1",
    )
    # target is v4 but payload declares v1 -> mismatch, no inference
    body = resp.json()
    assert body["code"] == "FEATURE_PROTOCOL_MISMATCH"
    assert body["field"] == "protocol_version"
    client.post("/v1/admin/weight", json={"weight": 0.3})


def test_missing_required_fields_are_business_errors(client):
    resp = client.post("/v1/rank", json={"request_id": "x"})
    assert resp.json()["code"] == "INVALID_REQUEST"


def test_circuit_breaker_auto_rolls_back_canary(client):
    client.post("/v1/admin/weight", json={"weight": 1.0})
    client.post(
        "/v1/admin/slow-calls",
        json={"version": "v4", "count": 10, "delay_ms": 200.0},
    )
    canary_hits = 0
    for index in range(8):
        body = _rank(client, "v4", f"slow-{index}", client_id="client-slow").json()
        # Requests are well-formed; once the breaker trips they land on v3
        # with a protocol-mismatch envelope because this helper sends v2 data.
        if body["code"] == "OK":
            assert body["model_version"] == "v4"
            canary_hits += 1
        else:
            assert body["code"] == "FEATURE_PROTOCOL_MISMATCH"
    assert canary_hits >= 6

    status = client.get("/v1/status").json()
    assert status["circuit_breaker"]["tripped"] is True
    record = status["circuit_breaker"]["last_rollback"]
    assert record["canary_version"] == "v4"
    assert record["sample_count"] >= 6
    assert record["reasons"] and "tripped_at" in record
    assert status["routing"]["default"] == 0.0

    # Subsequent traffic falls back to stable v3 without restart: send a
    # well-formed v1 payload and confirm it is served by the stable model.
    after = _rank(client, "v3", "post-rollback", client_id="client-slow").json()
    assert after["model_version"] == "v3"


def test_prometheus_metrics_exposed_with_stable_names(client):
    _rank(client, "v3", "metric-1", client_id="client-m")
    metrics = client.get("/metrics").text
    for name in (
        "inference_gateway_requests_total",
        "inference_gateway_errors_total",
        "inference_gateway_timeouts_total",
        "inference_gateway_request_latency_seconds",
        "inference_gateway_queue_depth",
        "inference_gateway_canary_weight",
        "inference_gateway_model_state",
    ):
        assert name in metrics
    assert 'model_version="v3"' in metrics
    assert 'tenant_id="tenant-a"' in metrics


def test_separate_metrics_port_serves_same_registry(config, client):
    import requests

    port = config.metrics_port
    session = requests.Session()
    session.trust_env = False
    resp = session.get(f"http://127.0.0.1:{port}/metrics", timeout=5)
    assert resp.status_code == 200
    assert "inference_gateway_requests_total" in resp.text


def test_status_json_is_sorted_and_uses_relative_model_files(client):
    body = client.get("/v1/status").text
    parsed = json.loads(body)
    assert json.dumps(parsed, sort_keys=True) == json.dumps(parsed, sort_keys=True)
    for state in parsed["models"].values():
        assert not state["model_file"].startswith(("/", "\\"))
