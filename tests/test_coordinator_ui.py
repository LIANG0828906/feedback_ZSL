from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request

import pytest

from lightprobe.bake import Baker
from lightprobe.coordinator import SceneRuntime
from lightprobe.errors import ProbeError
from lightprobe.ui import serve_in_thread

from conftest import fast_config


class BlockingBaker:
    def __init__(self) -> None:
        self.started = threading.Event()

    def bake(self, scene, config, **kwargs):
        self.started.set()
        cancel_event = kwargs.get("cancel_event")
        while not cancel_event.wait(0.01):
            pass
        raise ProbeError("E_CANCELLED", "cancelled in test")


def test_busy_and_cancel_keep_last_published_version(room):
    runtime = SceneRuntime(room, snapshot_root="artifacts/runs")
    real = Baker().bake(room, fast_config(room))
    published = runtime.store.publish(real.state, version=1)
    blocker = BlockingBaker()
    runtime.baker = blocker  # type: ignore[assignment]
    run = runtime.start_bake({"settings": {"compensation_strength": 0.7}})
    assert blocker.started.wait(5)
    with pytest.raises(ProbeError) as caught:
        runtime.start_bake({})
    assert caught.value.code == "E_BUSY"
    other_runtime = SceneRuntime(room, snapshot_root="artifacts/runs")
    with pytest.raises(ProbeError) as caught:
        other_runtime.start_bake({})
    assert caught.value.code == "E_BUSY"
    runtime.cancel()
    assert runtime.wait(5)
    status = runtime.status()
    assert status["phase"]["phase"] == "cancelled"
    assert status["last_error"]["code"] == "E_CANCELLED"
    assert runtime.store.current.version == published.version
    query = runtime.query([3.0, 1.0, 3.0], expected_version=1)
    assert query["version"] == 1


def test_http_ui_and_query_stale(room):
    runtime = SceneRuntime(room, snapshot_root="artifacts/runs")
    result = Baker().bake(room, fast_config(room))
    runtime.store.publish(result.state)
    server, thread = serve_in_thread(runtime, port=0)
    base = f"http://127.0.0.1:{server.server_port}"
    try:
        with urllib.request.urlopen(base + "/", timeout=5) as response:
            html = response.read().decode("utf-8")
        assert "Local Light Probe Baker" in html
        with urllib.request.urlopen(base + "/api/state", timeout=5) as response:
            state = json.loads(response.read())
        assert state["ok"]
        assert state["published"]["version"] == 1
        assert state["probes"]

        body = json.dumps(
            {
                "position": [3.0, 1.0, 3.0],
                "expected_version": 99,
                "expected_content_hash": state["published"]["content_hash"],
            }
        ).encode()
        request = urllib.request.Request(
            base + "/api/query",
            data=body,
            headers={"content-type": "application/json"},
            method="POST",
        )
        with pytest.raises(urllib.error.HTTPError) as caught:
            urllib.request.urlopen(request, timeout=5)
        payload = json.loads(caught.value.read())
        assert payload["code"] == "E_VERSION_STALE"
        assert caught.value.code == 409
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)

