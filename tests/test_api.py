from __future__ import annotations

import json
import threading
import urllib.error
import urllib.request

from release_orchestrator.api import create_server
from release_orchestrator.generator import generate_scenario
from release_orchestrator.service import Orchestrator


def request_json(url, *, method="GET", body=None):
    data = None if body is None else json.dumps(body).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        return response.status, json.loads(response.read())


def test_http_status_metrics_and_run(tmp_path):
    samples = tmp_path / "samples"
    generate_scenario(
        str(samples),
        scenario="clean",
        seed=20261119,
        force=True,
    )
    orchestrator = Orchestrator(str(tmp_path / "api.db"))
    server = create_server("127.0.0.1", 0, orchestrator)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        status, health = request_json(base + "/healthz")
        assert status == 200
        assert health["status"] == "healthy"

        status, release = request_json(
            base + "/api/releases",
            method="POST",
            body={"sample_dir": str(samples)},
        )
        assert status == 201
        release_id = release["release_id"]

        status, completed = request_json(
            base + f"/api/releases/{release_id}/run",
            method="POST",
            body={},
        )
        assert status == 200
        assert completed["status"] == "completed"
        assert completed["released_instance_count"] == 48

        status, fetched = request_json(
            base + f"/api/releases/{release_id}"
        )
        assert status == 200
        assert fetched["decision_sequence"][0]["result"] == "released"

        with urllib.request.urlopen(base + "/metrics", timeout=10) as response:
            metrics = response.read().decode("utf-8")
        assert "release_orchestrator_current_batch" in metrics
        assert "release_orchestrator_released_instances" in metrics
        assert "release_orchestrator_threshold_instances" in metrics
        assert "release_orchestrator_abort_count" in metrics
        assert "release_orchestrator_rollback_in_progress" in metrics
        assert "release_orchestrator_parameter_version" in metrics
        assert f'release_id="{release_id}"' in metrics
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_http_conflict_returns_business_error(tmp_path):
    samples = tmp_path / "samples"
    generate_scenario(
        str(samples),
        scenario="concurrent",
        seed=20261119,
        force=True,
    )
    orchestrator = Orchestrator(str(tmp_path / "api.db"))
    first = orchestrator.start_from_sample_dir(str(samples))
    orchestrator.evaluate_next(first["release_id"])
    server = create_server("127.0.0.1", 0, orchestrator)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        status, late = request_json(
            base + "/api/releases",
            method="POST",
            body={
                "plan_path": str(samples / "plan_conflict.json"),
                "metrics_path": str(samples / "metrics_conflict.jsonl"),
                "version_reports_path": str(
                    samples / "version_reports_conflict.jsonl"
                ),
                "base_dir": str(samples),
            },
        )
        assert status == 201
        with urllib.request.urlopen(
            urllib.request.Request(
                base + f"/api/releases/{late['release_id']}/evaluate",
                data=b"{}",
                method="POST",
                headers={"Content-Type": "application/json"},
            ),
            timeout=10,
        ) as response:
            raise AssertionError("expected HTTP 409")
    except urllib.error.HTTPError as error:
        payload = json.loads(error.read())
        assert error.code == 409
        assert payload["error"]["code"] == "INSTANCE_OCCUPANCY_CONFLICT"
        assert payload["error"]["conflict_release_ids"] == [
            first["release_id"]
        ]
        assert payload["release"]["batches"][0]["reason_code"] == (
            "INSTANCE_OCCUPANCY_CONFLICT"
        )
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
