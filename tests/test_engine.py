from __future__ import annotations

import tempfile
import threading
import time
import unittest
from pathlib import Path

import grpc

from inference_router.config import load_router_config
from inference_router.engine import InferenceEngine
from inference_router.health import HealthManager
from inference_router.metrics import Metrics
from inference_router.pb import inference_pb2
from inference_router.stats import RequestStats
from inference_router.store import RequestStore


class _RpcError(grpc.RpcError):
    def __init__(self, code: grpc.StatusCode) -> None:
        self._code = code

    def code(self):
        return self._code

    def details(self) -> str:
        return "injected failure"


class FakeStub:
    def __init__(
        self,
        replica_id: str,
        layer_id: str,
        output_value: float,
        *,
        delay_s: float = 0.0,
        error: grpc.StatusCode | None = None,
    ) -> None:
        self.replica_id = replica_id
        self.layer_id = layer_id
        self.output_value = output_value
        self.delay_s = delay_s
        self.error = error
        self.calls = []
        self.lock = threading.Lock()

    def Infer(self, request, timeout):
        with self.lock:
            self.calls.append(
                {
                    "request_id": request.request_id,
                    "session_id": request.session_id,
                }
            )
        if self.delay_s:
            time.sleep(self.delay_s)
        if self.error is not None:
            raise _RpcError(self.error)
        return inference_pb2.WorkerInferReply(
            ok=True,
            request_id=request.request_id,
            session_id=request.session_id,
            output=[self.output_value] * 64,
            replica_id=self.replica_id,
            layer_id=self.layer_id,
            compute_ms=1.0,
        )


class EngineTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.config = load_router_config("config/router.json")
        self.store = RequestStore(str(Path(self.temp.name) / "router.sqlite3"))
        self.stats = RequestStats(100, 120.0)
        self.health = HealthManager(
            list(self.config.shards),
            self.config.layer_by_id,
            self.config.health,
            lambda _event: None,
        )
        self.metrics = Metrics(
            self.health,
            self.stats,
            [layer.layer_id for layer in self.config.layers],
        )
        self.engine = InferenceEngine(
            self.config,
            self.store,
            self.health,
            self.stats,
            self.metrics,
        )

    def tearDown(self) -> None:
        self.health.close()
        self.store.close()
        self.temp.cleanup()

    def _install_stubs(self, stubs: dict[str, FakeStub]) -> None:
        for replica_id, stub in stubs.items():
            self.health.states[replica_id].stub = stub

    def test_second_tier_timeout_switches_with_same_session_and_single_adoption(self) -> None:
        l1 = FakeStub("l1-a", "l1", 0.1)
        l2_primary = FakeStub(
            "l2-a",
            "l2",
            0.2,
            error=grpc.StatusCode.DEADLINE_EXCEEDED,
        )
        l2_secondary = FakeStub("l2-b", "l2", 0.3)
        self._install_stubs({"l1-a": l1, "l2-a": l2_primary, "l2-b": l2_secondary})
        self.health.select_in_layer = lambda layer_id, excluded=None: (
            self.health.states["l1-a"]
            if layer_id == "l1"
            else self.health.states["l2-a"]
        )

        reply, deduplicated = self.engine.submit(
            "retry-request",
            "retry-session",
            [0.0] * 64,
        )

        self.assertFalse(deduplicated)
        self.assertTrue(reply["ok"])
        self.assertEqual(reply["l1_replica_id"], "l1-a")
        self.assertEqual(reply["l2_replica_id"], "l2-b")
        self.assertEqual(reply["retry_switches"], 1)
        self.assertEqual(reply["attempted_replicas"], ["l1-a", "l2-a", "l2-b"])
        self.assertEqual(len(reply["calls"]), 3)
        self.assertTrue(all(call["session_id"] == "retry-session" for call in reply["calls"]))
        self.assertEqual(len(l1.calls), 1)
        self.assertEqual(len(l2_primary.calls), 1)
        self.assertEqual(len(l2_secondary.calls), 1)
        stored = self.store.get_request("retry-request")
        self.assertEqual(len(stored["calls"]), 3)
        self.assertEqual(stored["events"][0]["event_type"], "retry_switch")
        self.assertEqual(stored["response"]["output"], reply["output"])

    def test_concurrent_duplicates_share_one_pipeline_execution(self) -> None:
        l1 = FakeStub("l1-a", "l1", 0.1, delay_s=0.03)
        l2 = FakeStub("l2-a", "l2", 0.2, delay_s=0.01)
        self._install_stubs({"l1-a": l1, "l2-a": l2})
        self.health.select_in_layer = lambda layer_id, excluded=None: (
            self.health.states["l1-a"]
            if layer_id == "l1"
            else self.health.states["l2-a"]
        )
        barrier = threading.Barrier(10)
        results = []
        results_lock = threading.Lock()

        def call_duplicate() -> None:
            barrier.wait()
            reply, deduplicated = self.engine.submit(
                "same-request",
                "same-session",
                [0.25] * 64,
            )
            with results_lock:
                results.append((reply, deduplicated))

        threads = [threading.Thread(target=call_duplicate) for _ in range(10)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=5)

        self.assertEqual(len(results), 10)
        self.assertEqual(len(l1.calls), 1)
        self.assertEqual(len(l2.calls), 1)
        self.assertEqual(len({tuple(reply["output"]) for reply, _ in results}), 1)
        self.assertEqual(sum(1 for _, dedup in results if not dedup), 1)
        self.assertEqual(len(self.store.get_request("same-request")["calls"]), 2)

    def test_failed_retry_returns_no_partial_output(self) -> None:
        l1 = FakeStub("l1-a", "l1", 0.1)
        l2_a = FakeStub(
            "l2-a", "l2", 0.2, error=grpc.StatusCode.UNAVAILABLE
        )
        l2_b = FakeStub(
            "l2-b", "l2", 0.3, error=grpc.StatusCode.UNAVAILABLE
        )
        self._install_stubs({"l1-a": l1, "l2-a": l2_a, "l2-b": l2_b})
        self.health.select_in_layer = lambda layer_id, excluded=None: (
            self.health.states["l1-a"]
            if layer_id == "l1"
            else self.health.states["l2-a"]
        )
        reply, _ = self.engine.submit(
            "failed-request",
            "failed-session",
            [0.0] * 64,
        )
        self.assertFalse(reply["ok"])
        self.assertEqual(reply["output"], [])
        self.assertEqual(reply["error_code"], "L2_ATTEMPTS_EXHAUSTED")
        self.assertEqual(reply["attempted_replicas"], ["l1-a", "l2-a", "l2-b"])
        self.assertEqual(len(reply["calls"]), 3)
        self.assertTrue(all(call["session_id"] == "failed-session" for call in reply["calls"]))


if __name__ == "__main__":
    unittest.main()
