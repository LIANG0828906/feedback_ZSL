from __future__ import annotations

import json
import hashlib
import math
import shutil
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

import onnxruntime as ort
import pytest
from fastapi.testclient import TestClient

from rec_gateway.assetgen import expected_score, generate_assets
from rec_gateway.main import create_app
from tests.conftest import copy_models, make_config, mutate_manifest, sample_for


def _assert_v3_contract(payload: dict[str, Any]) -> None:
    assert payload["ok"] is True
    assert payload["served_version"] == "v3"
    assert payload["protocol_version"] == "v3"
    assert isinstance(payload["score"], float)


def _assert_v4_contract(payload: dict[str, Any]) -> None:
    assert payload["ok"] is True
    assert payload["served_version"] == "v4"
    assert payload["protocol_version"] == "v4"
    assert set(payload["score"]) == {
        "calibrated",
        "calibration_offset",
        "calibration_version",
        "raw_score",
        "value",
    }
    assert isinstance(payload["score"]["value"], float)
    assert payload["score"]["calibrated"] is True


def test_sample_workload_covers_four_tenants_and_both_protocols(
    samples: list[dict[str, Any]],
) -> None:
    coverage: dict[tuple[str, str], int] = {}
    for sample in samples:
        key = (sample["tenant_id"], sample["protocol_version"])
        coverage[key] = coverage.get(key, 0) + 1
        expected_dimension = 48 if sample["protocol_version"] == "v3" else 56
        assert len(sample["features"]) == expected_dimension
        assert len(sample["feature_names"]) == expected_dimension
    for tenant_id in ("tenant-a", "tenant-b", "tenant-c", "tenant-d"):
        for protocol_version in ("v3", "v4"):
            assert coverage[(tenant_id, protocol_version)] > 0


def test_generated_models_match_manifest_and_oracle(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    expected = {"v3": (48, 1), "v4": (56, 2)}
    for version, (dimension, output_count) in expected.items():
        manifest = json.loads(
            (generated_root / "models" / version / "manifest.json").read_text()
        )
        session = ort.InferenceSession(
            str(generated_root / "models" / version / "model.onnx"),
            providers=["CPUExecutionProvider"],
        )
        assert manifest["feature_protocol"]["dimension"] == dimension
        assert session.get_inputs()[0].shape[1] == dimension
        assert len(session.get_outputs()) == output_count

    config = make_config(tmp_path, generated_root / "models")
    with TestClient(create_app(config=config)) as client:
        for version, tenant_id in (("v3", "tenant-a"), ("v4", "tenant-d")):
            request = sample_for(samples, version, tenant_id)
            request["request_id"] = f"oracle-{version}"
            response = client.post("/v1/infer", json=request)
            assert response.status_code == 200
            payload = response.json()
            feature_map = dict(zip(request["feature_names"], request["features"]))
            oracle = expected_score(version, feature_map)
            actual = (
                payload["score"]
                if version == "v3"
                else payload["score"]["raw_score"]
            )
            assert math.isclose(actual, oracle, rel_tol=0.0, abs_tol=2e-4)


def test_output_contract_isolation_and_mixed_batch(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    with TestClient(create_app(config=config)) as client:
        v3_request = sample_for(samples, "v3", "tenant-a", "client-v3")
        v4_request = sample_for(samples, "v4", "tenant-d", "client-v4")
        v3_request["request_id"] = "contract-v3"
        v4_request["request_id"] = "contract-v4"
        v3_response = client.post("/v1/infer", json=v3_request)
        v4_response = client.post("/v1/infer", json=v4_request)
        assert v3_response.status_code == 200
        assert v4_response.status_code == 200
        _assert_v3_contract(v3_response.json())
        _assert_v4_contract(v4_response.json())

        malformed = sample_for(samples, "v4", "tenant-d", "client-bad")
        malformed["request_id"] = "contract-bad"
        malformed["feature_names"][0], malformed["feature_names"][1] = (
            malformed["feature_names"][1],
            malformed["feature_names"][0],
        )
        batch = client.post(
            "/v1/infer/batch",
            json={"items": [v3_request, v4_request, malformed]},
        )
        assert batch.status_code == 200
        results = batch.json()["results"]
        assert len(results) == 3
        _assert_v3_contract(results[0])
        _assert_v4_contract(results[1])
        assert results[2]["ok"] is False
        assert results[2]["error"]["code"] == "FEATURE_COLUMN_ORDER_MISMATCH"


def test_feature_errors_are_precise_and_non_poisoning(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    with TestClient(create_app(config=config)) as client:
        base = sample_for(samples, "v4", "tenant-d", "fragile-client")

        count_request = dict(base)
        count_request["request_id"] = "bad-count"
        count_request["features"] = base["features"][:-1]
        count_request["feature_names"] = base["feature_names"][:-1]
        response = client.post("/v1/infer", json=count_request)
        assert response.status_code == 400
        body = response.json()
        assert body["error"]["code"] == "FEATURE_COUNT_MISMATCH"
        assert body["error"]["details"]["expected_count"] == 56
        assert body["error"]["details"]["actual_count"] == 55

        order_request = dict(base)
        order_request["request_id"] = "bad-order"
        order_request["feature_names"] = list(base["feature_names"])
        order_request["feature_names"][7], order_request["feature_names"][8] = (
            order_request["feature_names"][8],
            order_request["feature_names"][7],
        )
        response = client.post("/v1/infer", json=order_request)
        assert response.status_code == 400
        error = response.json()["error"]
        assert error["code"] == "FEATURE_COLUMN_ORDER_MISMATCH"
        assert error["details"]["column_index"] == 7
        assert error["details"]["expected_column"] == base["feature_names"][7]

        type_request = dict(base)
        type_request["request_id"] = "bad-type"
        type_request["features"] = list(base["features"])
        type_request["features"][3] = "not-a-number"
        response = client.post("/v1/infer", json=type_request)
        assert response.status_code == 400
        error = response.json()["error"]
        assert error["code"] == "FEATURE_TYPE_MISMATCH"
        assert error["details"]["column_index"] == 3

        valid_request = dict(base)
        valid_request["request_id"] = "valid-after-errors"
        response = client.post("/v1/infer", json=valid_request)
        assert response.status_code == 200
        _assert_v4_contract(response.json())
        assert client.get("/healthz").status_code == 200


def test_runtime_weight_change_and_client_audit(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    with TestClient(create_app(config=config)) as client:
        tenant_id = "tenant-c"
        client_id = "switch-client"
        v3_request = sample_for(samples, "v3", tenant_id, client_id)
        v4_request = sample_for(samples, "v4", tenant_id, client_id)

        response = client.put(
            f"/admin/tenants/{tenant_id}/weights",
            json={"canary_weight": 0.0},
        )
        assert response.status_code == 200
        v3_request["request_id"] = "switch-before"
        first = client.post("/v1/infer", json=v3_request)
        assert first.status_code == 200
        assert first.json()["served_version"] == "v3"

        response = client.put(
            f"/admin/tenants/{tenant_id}/weights",
            json={"canary_weight": 1.0},
        )
        assert response.status_code == 200
        preview = client.get(
            "/admin/route/preview",
            params={"tenant_id": tenant_id, "client_id": client_id},
        ).json()
        assert preview["candidate_version"] == "v4"
        v4_request["request_id"] = "switch-after"
        second = client.post("/v1/infer", json=v4_request)
        assert second.status_code == 200
        assert second.json()["served_version"] == "v4"

        response = client.put(
            f"/admin/tenants/{tenant_id}/weights",
            json={"canary_weight": 0.0},
        )
        assert response.status_code == 200
        v3_request["request_id"] = "switch-back"
        third = client.post("/v1/infer", json=v3_request)
        assert third.status_code == 200
        assert third.json()["served_version"] == "v3"

        audit = client.get(
            "/admin/route-audit",
            params={"tenant_id": tenant_id, "client_id": client_id},
        ).json()["events"]
        selected = {event["request_id"]: event["selected_version"] for event in audit}
        weights = {
            event["request_id"]: event["canary_weight"] for event in audit
        }
        assert selected["switch-before"] == "v3"
        assert selected["switch-after"] == "v4"
        assert selected["switch-back"] == "v3"
        assert weights["switch-before"] == 0.0
        assert weights["switch-after"] == 1.0
        assert weights["switch-back"] == 0.0


def test_rollback_on_p99_and_stable_remains_available(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    model_dir = copy_models(generated_root / "models", tmp_path / "rollback-models")

    def loosen_window(manifest: dict[str, Any]) -> None:
        manifest["slo"]["latency_p99_ms"] = 10.0
        manifest["slo"]["window_size"] = 20
        manifest["slo"]["min_samples"] = 5
        manifest["slo"]["error_rate"] = 0.5

    mutate_manifest(model_dir, "v4", loosen_window)
    config = make_config(
        tmp_path,
        model_dir,
        weights={"tenant-a": 0.0, "tenant-b": 0.0, "tenant-c": 0.0, "tenant-d": 1.0},
    )
    with TestClient(create_app(config=config)) as client:
        for index in range(5):
            request = sample_for(samples, "v4", "tenant-d", "slow-client")
            request["request_id"] = f"slow-{index}"
            request["fault_injection"] = {"inference_delay_ms": 25.0}
            response = client.post("/v1/infer", json=request)
            assert response.status_code == 200

        status = client.get("/status").json()
        assert len(status["recent_rollbacks"]) == 1
        rollback = status["recent_rollbacks"][0]
        assert rollback["version"] == "v4"
        assert rollback["metric"] == "p99_latency_ms"
        assert rollback["window_samples"] == 5
        assert rollback["observed"] > rollback["threshold"]
        assert status["routing"]["tenants"]["tenant-d"]["canary_weight"] == 0.0

        stable_request = sample_for(samples, "v3", "tenant-a", "stable-client")
        stable_request["request_id"] = "stable-during-rollback"
        stable = client.post("/v1/infer", json=stable_request)
        assert stable.status_code == 200
        assert stable.json()["served_version"] == "v3"

        metrics = client.get("/metrics").text
        assert "gateway_rollbacks_total" in metrics
        assert 'reason="p99_latency_exceeded"' in metrics


def test_timeout_is_counted_and_other_version_serves(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(
        tmp_path,
        generated_root / "models",
        weights={"tenant-a": 0.0, "tenant-b": 0.0, "tenant-c": 0.0, "tenant-d": 1.0},
        request_timeout_ms=40.0,
    )
    with TestClient(create_app(config=config)) as client:
        slow_request = sample_for(samples, "v4", "tenant-d", "timeout-client")
        slow_request["request_id"] = "timeout-1"
        slow_request["fault_injection"] = {"inference_delay_ms": 100.0}
        response = client.post("/v1/infer", json=slow_request)
        assert response.status_code == 504
        assert response.json()["error"]["code"] == "INFERENCE_TIMEOUT"

        stable_request = sample_for(samples, "v3", "tenant-a", "stable-timeout")
        stable_request["request_id"] = "stable-timeout"
        stable = client.post("/v1/infer", json=stable_request)
        assert stable.status_code == 200
        metrics = client.get("/metrics").text
        assert 'gateway_timeouts_total{tenant="tenant-d",version="v4"} 1.0' in metrics


def test_version_load_failure_is_isolated(
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    generated = tmp_path / "broken"
    generate_assets(generated, sample_count=20, inject_v4_load_failure=True)
    config = make_config(tmp_path, generated / "models")
    with TestClient(create_app(config=config)) as client:
        status = client.get("/status").json()
        assert status["versions"]["v3"]["status"] == "ready"
        assert status["versions"]["v4"]["status"] == "failed"
        assert status["versions"]["v4"]["error_code"] in {
            "MANIFEST_DIMENSION_MISMATCH",
            "MODEL_INPUT_CONTRACT_MISMATCH",
        }
        v3_request = sample_for(samples, "v3", "tenant-c", "load-failure")
        v3_request["request_id"] = "load-failure-v3"
        response = client.post("/v1/infer", json=v3_request)
        assert response.status_code == 200
        assert response.json()["served_version"] == "v3"

        client.put(
            "/admin/tenants/tenant-c/weights",
            json={"canary_weight": 1.0},
        )
        v3_request["request_id"] = "load-failure-fallback"
        fallback = client.post("/v1/infer", json=v3_request)
        assert fallback.status_code == 200
        assert fallback.json()["served_version"] == "v3"
        audit = client.get(
            "/admin/route-audit",
            params={"tenant_id": "tenant-c", "client_id": "load-failure"},
        ).json()["events"]
        assert audit[0]["candidate_version"] == "v4"
        assert audit[0]["selected_version"] == "v3"
        assert audit[0]["fallback_reason"] == "candidate_model_unavailable"


def test_output_dimension_mismatch_marks_version_failed(
    generated_root: Path,
    tmp_path: Path,
) -> None:
    model_dir = copy_models(generated_root / "models", tmp_path / "output-mismatch")
    model_path = model_dir / "v4" / "model.onnx"
    model_bytes = model_path.read_bytes()
    assert model_bytes.count(b"score") >= 1
    changed_model = model_bytes.replace(b"score", b"scor3")
    model_path.write_bytes(changed_model)
    mutate_manifest(
        model_dir,
        "v4",
        lambda manifest: manifest.__setitem__(
            "model_sha256", hashlib.sha256(changed_model).hexdigest()
        ),
    )
    config = make_config(tmp_path, model_dir)
    with TestClient(create_app(config=config)) as client:
        status = client.get("/status").json()
        assert status["versions"]["v3"]["status"] == "ready"
        assert status["versions"]["v4"]["status"] == "failed"
        assert (
            status["versions"]["v4"]["error_code"]
            == "MODEL_OUTPUT_CONTRACT_MISMATCH"
        )


def test_loading_state_is_observable(
    generated_root: Path,
    tmp_path: Path,
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    app = create_app(config=config)
    with TestClient(app) as client:
        models = app.state.models
        original = models._open_validated

        def slow_open(version: str):
            time.sleep(0.25)
            return original(version)

        models._open_validated = slow_open
        response = client.post("/admin/models/v4/reload")
        assert response.status_code == 202
        status = client.get("/status").json()
        assert status["versions"]["v4"]["status"] == "loading"
        deadline = time.time() + 3.0
        while time.time() < deadline:
            state = client.get("/status").json()["versions"]["v4"]["status"]
            if state == "ready":
                break
            time.sleep(0.05)
        assert state == "ready"


def test_sqlite_policy_idempotency_and_restart_recovery(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    request = sample_for(samples, "v3", "tenant-a", "restart-client")
    request["request_id"] = "restart-idempotent"
    with TestClient(create_app(config=config)) as client:
        first = client.post("/v1/infer", json=request)
        assert first.status_code == 200
        original_payload = first.json()
        client.put(
            "/admin/tenants/tenant-c/weights",
            json={"canary_weight": 0.75},
        )

    with TestClient(create_app(config=config)) as client:
        status = client.get("/status").json()
        assert status["routing"]["tenants"]["tenant-c"]["canary_weight"] == 0.75
        metrics = client.get("/metrics").text
        assert 'gateway_requests_total{tenant="tenant-a",version="v3"} 1.0' in metrics
        replay = client.post("/v1/infer", json=request)
        assert replay.status_code == 200
        assert replay.headers["x-idempotent-replay"] == "true"
        assert replay.json() == original_payload

        conflict_request = dict(request)
        conflict_request["features"] = list(request["features"])
        conflict_request["features"][0] += 0.5
        conflict = client.post("/v1/infer", json=conflict_request)
        assert conflict.status_code == 409
        assert conflict.json()["error"]["code"] == "IDEMPOTENCY_CONFLICT"


def test_concurrent_idempotent_requests_share_one_result(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    request = sample_for(samples, "v4", "tenant-d", "concurrent-client")
    request["request_id"] = "concurrent-idempotent"
    request["fault_injection"] = {"inference_delay_ms": 100.0}
    with TestClient(create_app(config=config)) as client:
        with ThreadPoolExecutor(max_workers=2) as executor:
            responses = list(
                executor.map(
                    lambda _: client.post("/v1/infer", json=request),
                    range(2),
                )
            )
        assert all(response.status_code == 200 for response in responses)
        assert responses[0].json() == responses[1].json()
        replays = [
            response.headers.get("x-idempotent-replay") for response in responses
        ]
        assert replays.count("true") == 1


def test_metrics_are_stable_and_status_has_no_absolute_paths(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(tmp_path, generated_root / "models")
    with TestClient(create_app(config=config)) as client:
        request = sample_for(samples, "v4", "tenant-d", "metrics-client")
        request["request_id"] = "metrics-1"
        client.post("/v1/infer", json=request)
        metrics = client.get("/metrics").text
        required = [
            "gateway_requests_total",
            "gateway_errors_total",
            "gateway_timeouts_total",
            "gateway_latency_p99_ms",
            "gateway_current_weight",
            "gateway_protocol_mismatch_rejections_total",
            "gateway_tenant_requests_total",
        ]
        for name in required:
            assert name in metrics
        assert 'tenant="tenant-d"' in metrics
        assert 'version="v4"' in metrics

        raw_status = client.get("/status").text
        assert str(tmp_path) not in raw_status
        assert str(generated_root) not in raw_status
        parsed = json.loads(raw_status)
        assert list(parsed) == sorted(parsed)


def test_batch_limit_returns_stable_error(
    generated_root: Path,
    tmp_path: Path,
    samples: list[dict[str, Any]],
) -> None:
    config = make_config(
        tmp_path,
        generated_root / "models",
        max_batch_size=2,
    )
    with TestClient(create_app(config=config)) as client:
        items = []
        for index in range(3):
            request = sample_for(samples, "v3", "tenant-a", f"batch-{index}")
            request["request_id"] = f"batch-limit-{index}"
            items.append(request)
        response = client.post("/v1/infer/batch", json={"items": items})
        assert response.status_code == 413
        assert response.json()["error"]["code"] == "BATCH_LIMIT_EXCEEDED"
