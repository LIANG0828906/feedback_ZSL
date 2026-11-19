from __future__ import annotations

import argparse
import json
import logging
import threading
import time
import traceback
from pathlib import Path
from typing import Any

import grpc
import numpy as np

from proto import training_pb2 as pb
from proto import training_pb2_grpc as pb_grpc
from train import db
from train.client import make_channel, retry_rpc
from train.common import (
    atomic_write_json,
    load_config,
    load_data_metadata,
    load_manifest_for_config,
    open_shard,
    parameter_names,
    resolve_path,
    step_batch_slice,
)
from train.model import loss_and_gradient


LOG = logging.getLogger("worker")


class TrainingWorker:
    def __init__(self, config_path: str, worker_id: str):
        self.config = load_config(config_path)
        self.manifest = load_manifest_for_config(self.config)
        self.data_metadata = load_data_metadata(self.config)
        worker_config = next(
            (
                item
                for item in self.config["workers"]
                if item["worker_id"] == worker_id
            ),
            None,
        )
        if worker_config is None:
            raise ValueError(f"unknown worker id {worker_id!r}")
        self.worker_id = worker_id
        self.shard_index = int(worker_config["shard_index"])
        shard_metadata = self.data_metadata["shards"][self.shard_index]
        self.sample_count = int(shard_metadata["sample_count"])
        self.features, self.labels = open_shard(
            self.config, self.data_metadata, self.shard_index
        )
        self.param_names = parameter_names(self.manifest)
        self.samples_per_step = int(
            self.manifest["hyperparameters"]["samples_per_worker_per_step"]
        )
        self.steps = int(self.manifest["hyperparameters"]["steps"])
        self.runtime = self.config["runtime"]
        coordinator = self.config["coordinator"]
        self.coordinator_endpoint = (
            f"{coordinator['host']}:{coordinator['port']}"
        )
        self.channel = make_channel(self.coordinator_endpoint)
        self.coordinator = pb_grpc.CoordinatorStub(self.channel)
        self.shard_channels: dict[str, grpc.Channel] = {}
        self.shard_stubs: dict[str, pb_grpc.ParameterShardStub] = {}
        self.stop_event = threading.Event()
        self.local_step = 0
        self.stale_responses = 0
        self.rejected_other = 0
        self.duplicate_responses = 0
        self.latencies_ms: list[float] = []
        self.rpc_retry_seconds = float(self.runtime["grpc_retry_seconds"])

    def close(self) -> None:
        self.stop_event.set()
        for channel in self.shard_channels.values():
            channel.close()
        self.shard_channels.clear()
        self.shard_stubs.clear()
        self.channel.close()

    def register_forever(self) -> dict[str, Any]:
        def operation():
            return self.coordinator.RegisterWorker(
                pb.RegisterWorkerRequest(
                    worker_id=self.worker_id,
                    data_shard_index=self.shard_index,
                    sample_count=self.sample_count,
                    timestamp_ms=int(time.time() * 1000),
                ),
                timeout=5.0,
            )

        reply = retry_rpc(
            operation,
            timeout_seconds=self.rpc_retry_seconds,
            description=f"register worker {self.worker_id}",
            stop_event=self.stop_event,
        )
        if not reply.ok:
            raise RuntimeError(f"worker registration failed: {reply.code} {reply.message}")
        LOG.info("registered %s shard=%s samples=%s", self.worker_id, self.shard_index, self.sample_count)
        return json.loads(reply.record_json) if reply.record_json else {}

    def start_heartbeat(self) -> threading.Thread:
        interval = (
            float(self.runtime["worker_heartbeat_interval_ms"]) / 1000.0
        )

        def loop():
            while not self.stop_event.is_set():
                try:
                    reply = self.coordinator.Heartbeat(
                        pb.HeartbeatRequest(
                            identity_type="worker",
                            identity_id=self.worker_id,
                            local_step=self.local_step,
                            timestamp_ms=int(time.time() * 1000),
                        ),
                        timeout=2.0,
                    )
                    if not reply.ok:
                        self.register_forever()
                except (grpc.RpcError, TimeoutError):
                    try:
                        self.register_forever()
                    except (grpc.RpcError, TimeoutError, RuntimeError):
                        pass
                self.stop_event.wait(interval)

        thread = threading.Thread(
            target=loop, name=f"heartbeat-{self.worker_id}", daemon=True
        )
        thread.start()
        return thread

    def _runtime(self, timeout_ms: int = 30000) -> pb.RuntimeReply:
        return self.coordinator.GetRuntime(
            pb.RuntimeRequest(timeout_ms=timeout_ms), timeout=timeout_ms / 1000.0 + 5.0
        )

    def _wait_for_version(self, target: int, timeout_ms: int = 30000) -> pb.WaitForVersionReply:
        return self.coordinator.WaitForVersion(
            pb.WaitForVersionRequest(target_version=target, timeout_ms=timeout_ms),
            timeout=timeout_ms / 1000.0 + 5.0,
        )

    def _stub_for(self, endpoint: str) -> pb_grpc.ParameterShardStub:
        stub = self.shard_stubs.get(endpoint)
        if stub is None:
            channel = make_channel(endpoint)
            self.shard_channels[endpoint] = channel
            stub = pb_grpc.ParameterShardStub(channel)
            self.shard_stubs[endpoint] = stub
        return stub

    def fetch_parameters(
        self, version: int, runtime: pb.RuntimeReply
    ) -> tuple[np.ndarray, int] | None:
        if not runtime.ok or runtime.takeover_in_progress:
            return None
        assignments = list(runtime.shards)
        expected = set(self.param_names)
        assigned = {
            name for assignment in assignments for name in assignment.parameter_names
        }
        if assigned != expected:
            return None
        values: dict[str, float] = {}
        actual_version = version
        for assignment in assignments:
            if not assignment.parameter_names:
                continue
            try:
                reply = self._stub_for(assignment.endpoint).GetParameters(
                    pb.GetParametersRequest(
                        shard_id=assignment.shard_id,
                        parameter_names=assignment.parameter_names,
                        version=version,
                    ),
                    timeout=3.0,
                )
            except grpc.RpcError:
                return None
            if not reply.ok:
                return None
            actual_version = int(reply.version)
            for parameter in reply.parameters:
                values[parameter.name] = float(parameter.value)
        if set(values) != expected:
            missing = sorted(expected - set(values))[:5]
            LOG.warning("parameter fetch incomplete, missing=%s", missing)
            return None
        return (
            np.asarray([values[name] for name in self.param_names], dtype=np.float64),
            actual_version,
        )

    def _compute(self, step: int, parameters: np.ndarray) -> tuple[np.ndarray, float]:
        batch = step_batch_slice(step, self.samples_per_step, self.sample_count)
        features = np.asarray(self.features[batch], dtype=np.float32)
        labels = np.asarray(self.labels[batch], dtype=np.int64)
        loss, gradient = loss_and_gradient(
            features, labels, parameters, self.manifest
        )
        return gradient, loss

    def _submit(
        self, step: int, base_version: int, gradient: np.ndarray, loss: float
    ) -> tuple[pb.GradientReply, float]:
        started = time.perf_counter()
        reply = self.coordinator.SubmitGradient(
            pb.GradientSubmission(
                worker_id=self.worker_id,
                step=step,
                base_version=base_version,
                sample_count=self.samples_per_step,
                gradient=[float(value) for value in gradient],
                local_loss=float(loss),
                client_started_ms=time.time() * 1000.0,
            ),
            timeout=10.0,
        )
        latency_ms = (time.perf_counter() - started) * 1000.0
        self.latencies_ms.append(latency_ms)
        return reply, latency_ms

    def run(self) -> dict[str, Any]:
        self.register_forever()
        self.start_heartbeat()
        started = time.perf_counter()
        step = 1
        while step <= self.steps and not self.stop_event.is_set():
            self.local_step = step - 1
            try:
                runtime = retry_rpc(
                    lambda: self._runtime(2000),
                    timeout_seconds=self.rpc_retry_seconds,
                    description="fetch runtime assignments",
                    stop_event=self.stop_event,
                )
            except TimeoutError:
                continue
            if not runtime.ok or runtime.version < step - 1:
                try:
                    self._wait_for_version(step - 1, timeout_ms=2000)
                except grpc.RpcError:
                    time.sleep(0.05)
                continue
            fetched = self.fetch_parameters(runtime.version, runtime)
            if fetched is None:
                time.sleep(0.05)
                continue
            parameters, base_version = fetched
            gradient, loss = self._compute(step, parameters)
            try:
                reply, _latency = self._submit(step, base_version, gradient, loss)
            except grpc.RpcError:
                time.sleep(0.05)
                continue
            if reply.code == "STALE_VERSION":
                self.stale_responses += 1
                time.sleep(0.02)
                continue
            if reply.code == "PIPELINE_BACKPRESSURE":
                self.rejected_other += 1
                time.sleep(0.05)
                continue
            if reply.code == "INVALID_BASE_VERSION":
                self.rejected_other += 1
                continue
            if reply.discarded and reply.code == "DUPLICATE_GRADIENT":
                self.duplicate_responses += 1
            if not reply.ok:
                self.rejected_other += 1
                LOG.warning(
                    "submission rejected worker=%s step=%s code=%s message=%s",
                    self.worker_id,
                    step,
                    reply.code,
                    reply.message,
                )
                time.sleep(0.05)
                continue
            if reply.applied_version >= step:
                step += 1
                continue
            wait_started = time.monotonic()
            while not self.stop_event.is_set():
                try:
                    waited = self._wait_for_version(step, timeout_ms=2000)
                except grpc.RpcError:
                    time.sleep(0.05)
                    continue
                if waited.version >= step:
                    step += 1
                    break
                if time.monotonic() - wait_started > float(
                    self.runtime["submit_wait_seconds"]
                ):
                    LOG.warning(
                        "step %s still pending after %.1fs; retrying submission",
                        step,
                        time.monotonic() - wait_started,
                    )
                    break
        self.local_step = step - 1
        elapsed = time.perf_counter() - started
        latencies = np.asarray(self.latencies_ms, dtype=np.float64)
        report = {
            "worker_id": self.worker_id,
            "data_shard_index": self.shard_index,
            "sample_count": self.sample_count,
            "completed_step": self.local_step,
            "elapsed_seconds": round(elapsed, 3),
            "submissions": int(latencies.size),
            "stale_responses": self.stale_responses,
            "duplicate_responses": self.duplicate_responses,
            "other_rejections": self.rejected_other,
            "submit_latency_p50_ms": (
                round(float(np.percentile(latencies, 50)), 3)
                if latencies.size
                else None
            ),
            "submit_latency_p95_ms": (
                round(float(np.percentile(latencies, 95)), 3)
                if latencies.size
                else None
            ),
            "submit_latency_p99_ms": (
                round(float(np.percentile(latencies, 99)), 3)
                if latencies.size
                else None
            ),
            "submit_latency_max_ms": (
                round(float(np.max(latencies)), 3) if latencies.size else None
            ),
        }
        report_path = resolve_path("run/worker_reports") / f"{self.worker_id}.json"
        atomic_write_json(report_path, report)
        return report


def main() -> int:
    parser = argparse.ArgumentParser(description="Run one asynchronous training worker")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--worker-id", required=True)
    args = parser.parse_args()
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    worker = TrainingWorker(args.config, args.worker_id)
    try:
        report = worker.run()
        print(json.dumps(report, sort_keys=True), flush=True)
        return 0 if report["completed_step"] >= worker.steps else 2
    finally:
        worker.close()


if __name__ == "__main__":
    raise SystemExit(main())
