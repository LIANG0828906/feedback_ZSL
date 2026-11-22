from __future__ import annotations

import copy
import json
import threading
import urllib.error
import urllib.request
from pathlib import Path

import numpy as np
import pytest

from transparent_pipeline.comparison import assert_equal_results, compare_results
from transparent_pipeline.engine import RenderEngine
from transparent_pipeline.opengl_probe import probe_opengl
from transparent_pipeline.renderer import RenderConfig
from transparent_pipeline.scene import Scene
from transparent_pipeline.errors import PipelineError
from transparent_pipeline.ui import Dashboard, create_dashboard_server, export_static_dashboard
from transparent_pipeline.verify import verify_three_paths


def config() -> RenderConfig:
    return RenderConfig(step_size=0.05)


def test_three_paths_and_truth(scene, truth, tmp_path):
    report = verify_three_paths(scene, config(), work_directory=tmp_path / "verify")
    assert report["ok"] is True, json.dumps(report, indent=2)
    assert report["code"] is None
    assert report["crashed_as_requested"] is True
    assert report["comparisons"]["full_vs_preview"]["equal"] is True
    assert report["comparisons"]["full_vs_resume"]["equal"] is True
    assert report["paths"]["full"]["commit_order"] == list(range(9))
    assert (tmp_path / "verify" / "verification.json").exists()


def test_divergence_localization(scene, tmp_path):
    expected = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "a").render(mode="full")
    actual = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "b").render(mode="full")
    actual.scattering[13, 17, 1] += 1.0e-9
    report = compare_results(actual, expected)
    assert report.equal is False
    divergence = report.first_divergence
    assert divergence["pixel"] == [17, 13]
    assert divergence["block_id"] == 4
    assert divergence["field"] == "scattering"
    assert divergence["wavelength_index"] == 1
    assert report.to_dict()["error"]["code"] == "E_DIVERGE"
    with pytest.raises(PipelineError) as caught:
        assert_equal_results(actual, expected)
    assert caught.value.code == "E_DIVERGE"
    assert caught.value.field == "scattering"
    assert caught.value.details["pixel"] == [17, 13]


def test_static_dashboard_export(scene, truth, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    outcome = engine.render(mode="full")
    files = outcome.export(tmp_path / "export", include_arrays=True)
    files.update(export_static_dashboard(engine, outcome, tmp_path / "export", sample_index=0))
    for path in files.values():
        assert Path(path).exists() and Path(path).stat().st_size > 0
    html = Path(files["dashboard"]).read_text(encoding="utf-8")
    assert "介质边界与透射率热力图" in html
    assert "分块状态矩阵" in html
    assert "失效传播清单" in html
    assert "差异热力图" in html
    state = json.loads(Path(files["state"]).read_text(encoding="utf-8"))
    assert state["progress"]["committed"] == 9
    assert state["transmission_heatmap"]
    assert state["boundaries"]


def test_dashboard_incremental_and_compare(scene, tmp_path):
    dashboard = Dashboard(
        scene,
        config(),
        tmp_path / "cp",
        tmp_path / "export",
        sample_index=0,
    )
    initial_hash = dashboard.state()["scene_hash"]
    edited = dashboard.render_action(
        {
            "action": "edit_medium",
            "medium_id": "glass_sphere",
            "absorption_scale": 1.1,
            "scattering_scale": 1.0,
        }
    )
    assert edited["ok"] is True
    assert edited["scene_hash"] != initial_hash
    assert edited["invalidation_manifest"]
    compared = dashboard.compare_full()
    assert compared["equal"] is True
    exported = dashboard.export_all()
    assert exported["ok"] is True
    assert Path(exported["files"]["dashboard"]).exists()


def test_opengl_probe_declares_software_fallback():
    report = probe_opengl()
    assert report["renderer"] == "software"
    assert report["software_fallback"] is True


def test_local_dashboard_http_api(scene, tmp_path):
    server, dashboard = create_dashboard_server(
        scene,
        config(),
        checkpoint_directory=tmp_path / "cp",
        export_directory=tmp_path / "export",
        port=0,
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        with urllib.request.urlopen(f"{base}/api/state", timeout=10) as response:
            state = json.loads(response.read().decode("utf-8"))
        assert state["progress"]["committed"] == 9
        request = urllib.request.Request(
            f"{base}/api/render",
            data=json.dumps(
                {
                    "action": "edit_medium",
                    "medium_id": "glass_sphere",
                    "absorption_scale": 1.1,
                    "scattering_scale": 1.0,
                }
            ).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            edited = json.loads(response.read().decode("utf-8"))
        assert edited["ok"] is True
        assert edited["invalidation_manifest"]
        export_request = urllib.request.Request(
            f"{base}/api/export",
            data=b"{}",
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(export_request, timeout=10) as response:
            exported = json.loads(response.read().decode("utf-8"))
        assert exported["ok"] is True
        assert all(Path(path).exists() for path in exported["files"].values())
        try:
            urllib.request.urlopen(f"{base}/does-not-exist", timeout=10)
        except urllib.error.HTTPError as exc:
            assert exc.code == 404
        else:
            raise AssertionError("missing endpoint should return HTTP 404")
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)

