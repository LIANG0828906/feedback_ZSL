from __future__ import annotations

import copy
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .checkpoint import CheckpointLoad, CheckpointManager
from .compare import ComparisonReport, compare_paths
from .engine import EngineConfig, SessionEngine
from .models import RunResult, SampleSession
from .negotiation import negotiate
from .transport import LoopbackTransport


@dataclass
class PathExecution:
    realtime: RunResult
    recovery: RunResult
    replay: RunResult
    comparison: ComparisonReport
    recovery_checkpoint: CheckpointLoad
    checkpoint_path: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "realtime": self.realtime.to_dict(),
            "recovery": self.recovery.to_dict(),
            "replay": self.replay.to_dict(),
            "comparison": self.comparison.to_dict(),
            "recovery_checkpoint": {
                "status": self.recovery_checkpoint.status,
                "reason": self.recovery_checkpoint.reason,
                "path": self.recovery_checkpoint.path,
            },
            "checkpoint_path": self.checkpoint_path,
        }


def _copy_config(config: EngineConfig) -> EngineConfig:
    return EngineConfig(
        retry_limit=config.retry_limit,
        checkpoint_every=config.checkpoint_every,
        max_parallelism=config.max_parallelism,
        per_thread_buffer=config.per_thread_buffer,
        anomaly=copy.deepcopy(config.anomaly),
        enable_checkpoints=config.enable_checkpoints,
        initial_tier=config.initial_tier,
    )


def run_realtime(
    sample: SampleSession,
    config: EngineConfig,
    transport: LoopbackTransport | None = None,
) -> RunResult:
    with SessionEngine(
        sample,
        config=_copy_config(config),
        transport=transport,
    ) as engine:
        engine.run_to_end()
        return engine.to_run_result("realtime")


def run_replay(
    sample: SampleSession,
    config: EngineConfig,
    transport: LoopbackTransport | None = None,
) -> RunResult:
    with SessionEngine(
        sample,
        config=_copy_config(config),
        transport=transport,
    ) as engine:
        engine.run_to_end()
        return engine.to_run_result("replay")


def run_recovery(
    sample: SampleSession,
    config: EngineConfig,
    checkpoint_directory: str | Path,
    recovery_after_frames: int = 18,
    transport_factory: Callable[[], LoopbackTransport] | None = None,
) -> tuple[RunResult, CheckpointLoad, str]:
    negotiation = negotiate(sample)
    manager = CheckpointManager(
        checkpoint_directory,
        session_id=sample.session_id,
        content_hash=sample.content_hash,
        capabilities_hash=negotiation.capabilities_hash,
        protocol_version=sample.protocol_version,
    )
    manager.clear()
    with SessionEngine(
        sample,
        config=_copy_config(config),
        checkpoint_manager=manager,
        negotiation=negotiation,
        transport=transport_factory() if transport_factory else None,
    ) as first:
        first.run_until_committed(recovery_after_frames)
        first.save_checkpoint()
    load = manager.load()
    if load.status != "loaded" or load.payload is None:
        raise RuntimeError(f"检查点恢复失败: {load.status} {load.reason}")
    with SessionEngine(
        sample,
        config=_copy_config(config),
        checkpoint_manager=manager,
        negotiation=negotiation,
        transport=transport_factory() if transport_factory else None,
    ) as restored:
        if not restored.restore(load.payload):
            raise RuntimeError("检查点 payload 校验失败，已拒绝部分恢复")
        restored.run_to_end()
        return restored.to_run_result("recovery"), load, str(manager.path)


def execute_paths(
    sample: SampleSession,
    config: EngineConfig | None = None,
    checkpoint_directory: str | Path = "artifacts/checkpoints/demo",
    recovery_after_frames: int = 18,
    transport_factory: Callable[[], LoopbackTransport] | None = None,
) -> PathExecution:
    active_config = config or EngineConfig()
    realtime = run_realtime(
        sample,
        active_config,
        transport=transport_factory() if transport_factory else None,
    )
    recovery, load, checkpoint_path = run_recovery(
        sample,
        active_config,
        checkpoint_directory,
        recovery_after_frames,
        transport_factory=transport_factory,
    )
    replay = run_replay(
        sample,
        active_config,
        transport=transport_factory() if transport_factory else None,
    )
    comparison = compare_paths(realtime, recovery, replay)
    return PathExecution(
        realtime=realtime,
        recovery=recovery,
        replay=replay,
        comparison=comparison,
        recovery_checkpoint=load,
        checkpoint_path=checkpoint_path,
    )
