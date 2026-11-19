"""HTTP-level readiness semantics: no 503, distinct load/wait error codes."""

import json
import shutil


def test_failed_version_receives_no_traffic_and_other_version_serves(
    generated_dir, tmp_path
):
    from fastapi.testclient import TestClient

    from gateway.app import create_app
    from gateway.config import GatewayConfig
    from tests.conftest import VERSION_CFG, _free_port

    broken = tmp_path / "models"
    broken.mkdir()
    mapping = json.loads(
        (generated_dir / "models" / "protocol_mapping.json").read_text()
    )
    mapping["v4"]["model_file"] = "missing-v4.onnx"
    (broken / "protocol_mapping.json").write_text(json.dumps(mapping))
    shutil.copy(
        generated_dir / "models" / "ranker_v3.onnx",
        broken / "ranker_v3.onnx",
    )

    cfg = GatewayConfig(
        port=_free_port(),
        metrics_port=_free_port(),
        model_dir=str(broken),
        default_canary_weight=1.0,
        versions=VERSION_CFG,
    )
    with TestClient(create_app(cfg)) as client:
        status = client.get("/v1/status").json()
        assert status["models"]["v4"]["state"] == "failed"
        assert status["models"]["v3"]["state"] == "ready"

        # Canary is unavailable despite weight 1.0: traffic falls back to v3,
        # and the response is always HTTP 200 (never 503).
        payload = {
            "request_id": "fallback-1",
            "tenant_id": "tenant-a",
            "client_id": "c-fallback",
            "protocol_version": "rank-features-v1",
            "columns": [
                "c_user_age", "c_user_tenure", "c_item_age", "c_item_price",
                "c_ctr_7d", "c_cvr_7d", "c_match_score", "c_dwell_mean",
            ],
            "features": [0.1] * 8,
        }
        response = client.post("/v1/rank", json=payload)
        assert response.status_code == 200
        assert response.json()["model_version"] == "v3"

        # Restore the v4 file and reload: failed version becomes ready again.
        mapping["v4"]["model_file"] = "ranker_v4.onnx"
        (broken / "protocol_mapping.json").write_text(json.dumps(mapping))
        shutil.copy(
            generated_dir / "models" / "ranker_v4.onnx",
            broken / "ranker_v4.onnx",
        )
        reload_resp = client.post(
            "/v1/admin/reload", json={"version": "v4"}
        ).json()
        assert reload_resp["model"]["state"] == "ready"

        v4_payload = {
            "request_id": "recovered-1",
            "tenant_id": "tenant-a",
            "client_id": "c-recovered",
            "protocol_version": "rank-features-v2",
            "columns": [
                "c_item_price", "c_item_age", "c_match_score", "c_ctr_7d",
                "c_cvr_7d", "c_dwell_mean", "c_user_age", "c_user_tenure",
                "c_emb_cosine", "c_ctx_hour",
            ],
            "features": [0.1] * 10,
        }
        recovered = client.post("/v1/rank", json=v4_payload).json()
        assert recovered["code"] == "OK"
        assert recovered["model_version"] == "v4"


def test_status_json_keys_are_stably_sorted(client):
    response = client.get("/v1/status")
    raw = response.text
    parsed = json.loads(raw)
    # Re-serializing with sort_keys must reproduce the exact wire bytes,
    # proving the server itself emits stably ordered field names.
    assert json.dumps(parsed, sort_keys=True) == raw
    assert "/tmp/" not in raw
    assert "\\" not in raw
