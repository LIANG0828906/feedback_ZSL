"""Isolated batching queues: full/timeout errors, slow calls and isolation."""

import asyncio
import threading

import numpy as np
import pytest

from gateway.config import VersionConfig
from gateway.errors import ErrorCode, GatewayError
from gateway.models import ModelRegistry
from gateway.pipeline import VersionPipeline
from gateway.protocol import load_protocol_mappings


def _pipeline(generated_dir, version, **overrides):
    model_dir = generated_dir / "models"
    specs = load_protocol_mappings(model_dir / "protocol_mapping.json")
    registry = ModelRegistry(model_dir, specs)
    registry.load_blocking(version)
    cfg = VersionConfig(**overrides)
    return VersionPipeline(version, cfg, registry)


async def _happy_path(generated_dir):
    pipeline = _pipeline(
        generated_dir, "v3", max_queue_depth=8, max_concurrency=1,
        min_batch_size=1, max_batch_size=2, min_wait_ms=1.0, max_wait_ms=4.0,
        enqueue_timeout_ms=500.0,
    )
    pipeline.start()
    try:
        vector = np.zeros(8, dtype=np.float32)
        scores = await asyncio.gather(
            *[pipeline.submit(f"r{i}", "t", "c", vector) for i in range(6)]
        )
        assert all(isinstance(score, float) for score in scores)
        assert pipeline.metrics.snapshot()["total_batches"] >= 1
    finally:
        await pipeline.stop()


def test_happy_path(generated_dir):
    asyncio.run(_happy_path(generated_dir))


async def _queue_full_and_timeout(generated_dir):
    # A blocking gate holds the single inference slot open for the whole test,
    # making backpressure deterministic and independent of wall-clock jitter.
    gate = threading.Event()

    def blocked_infer(session, matrix):
        gate.wait(timeout=5.0)
        return np.zeros(matrix.shape[0], dtype=np.float32)

    pipeline = _pipeline(
        generated_dir,
        "v4",
        max_queue_depth=4,
        max_concurrency=1,
        min_batch_size=8,
        max_batch_size=8,
        min_wait_ms=400.0,
        max_wait_ms=400.0,
        enqueue_timeout_ms=100.0,
    )
    pipeline._infer_sync = blocked_infer  # type: ignore[assignment]
    pipeline.start()
    vector = np.zeros(10, dtype=np.float32)
    try:
        pending = [
            asyncio.create_task(pipeline.submit(f"slow-{i}", "t", "c", vector))
            for i in range(4)
        ]
        # Wait until all four admission slots are occupied (queued + in-flight).
        deadline = asyncio.get_running_loop().time() + 1.0
        while pipeline._admitted < 4 and asyncio.get_running_loop().time() < deadline:
            await asyncio.sleep(0.01)
        assert pipeline._admitted == 4

        with pytest.raises(GatewayError) as full:
            await pipeline.submit("overflow", "t", "c", vector)
        assert full.value.code == ErrorCode.QUEUE_FULL

        # Cancel one pending request to free an admission slot, then enqueue a
        # fresh request there. The inference slot stays blocked, so once admitted
        # it can only fail with QUEUE_WAIT_TIMEOUT, never succeed.
        pending[0].cancel()
        deadline = asyncio.get_running_loop().time() + 1.0
        while pipeline._admitted >= 4 and asyncio.get_running_loop().time() < deadline:
            await asyncio.sleep(0.01)

        gate_task = asyncio.create_task(pipeline.submit("late", "t", "c", vector))
        with pytest.raises(GatewayError) as timeout:
            await asyncio.wait_for(gate_task, timeout=0.6)
        assert timeout.value.code == ErrorCode.QUEUE_WAIT_TIMEOUT

        gate.set()
        for task in pending:
            if not task.done():
                task.cancel()
    finally:
        gate.set()
        await pipeline.stop()


def test_queue_full_and_timeout(generated_dir):
    asyncio.run(_queue_full_and_timeout(generated_dir))


async def _isolation(generated_dir):
    gate = threading.Event()

    def blocked_infer(session, matrix):
        gate.wait(timeout=5.0)
        return np.zeros(matrix.shape[0], dtype=np.float32)

    slow = _pipeline(
        generated_dir, "v4", max_queue_depth=16, max_concurrency=1,
        min_batch_size=1, max_batch_size=1, min_wait_ms=1.0, max_wait_ms=1.0,
        enqueue_timeout_ms=2000.0,
    )
    slow._infer_sync = blocked_infer  # type: ignore[assignment]
    fast = _pipeline(
        generated_dir, "v3", max_queue_depth=16, max_concurrency=1,
        min_batch_size=1, max_batch_size=1, min_wait_ms=1.0, max_wait_ms=1.0,
        enqueue_timeout_ms=2000.0,
    )
    slow.start()
    fast.start()
    try:
        slow_tasks = [
            asyncio.create_task(slow.submit(f"s{i}", "t", "c", np.zeros(10, np.float32)))
            for i in range(4)
        ]
        deadline = asyncio.get_running_loop().time() + 1.0
        while slow._admitted < 4 and asyncio.get_running_loop().time() < deadline:
            await asyncio.sleep(0.01)
        # v3 queue stays empty and requests complete promptly despite v4 backlog
        assert fast.metrics.snapshot()["depth"] == 0
        v3_vector = np.zeros(8, dtype=np.float32)
        loop = asyncio.get_running_loop()
        start = loop.time()
        await fast.submit("fast-1", "t", "c", v3_vector)
        elapsed_ms = (loop.time() - start) * 1000.0
        assert elapsed_ms < 200.0
        gate.set()
        for task in slow_tasks:
            if not task.done():
                task.cancel()
    finally:
        gate.set()
        await slow.stop()
        await fast.stop()


def test_version_queues_are_isolated(generated_dir):
    asyncio.run(_isolation(generated_dir))
