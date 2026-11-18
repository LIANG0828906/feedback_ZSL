from __future__ import annotations

import copy
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Mapping

from .errors import BusinessError, E_CONFIG, E_PROGRESS_RANGE
from .util import canonical_json_bytes, sha256_bytes


SHARD_COUNT = 4


@dataclass(frozen=True)
class FaultConfig:
    enabled: bool
    shard_id: int
    after_step: int
    outage_steps: int
    stall_tolerance_steps: int
    replacement_id: str
    replacement_delay_ms: int


@dataclass
class Config:
    raw: Dict[str, Any]
    source_path: Path
    run_dir: Path

    @property
    def seed(self) -> int:
        return int(self.raw["run"]["seed"])

    @property
    def host(self) -> str:
        return str(self.raw["run"]["host"])

    @property
    def port(self) -> int:
        return int(self.raw["run"]["port"])

    @property
    def input_dim(self) -> int:
        return int(self.raw["model"]["input_dim"])

    @property
    def hidden_dim(self) -> int:
        return int(self.raw["model"]["hidden_dim"])

    @property
    def output_dim(self) -> int:
        return int(self.raw["model"]["output_dim"])

    @property
    def sample_count(self) -> int:
        return int(self.raw["training"]["sample_count"])

    @property
    def eval_samples(self) -> int:
        return int(self.raw["training"].get("eval_samples", 4096))

    @property
    def batch_size(self) -> int:
        return int(self.raw["training"]["batch_size"])

    @property
    def local_batch_size(self) -> int:
        return self.batch_size // SHARD_COUNT

    @property
    def total_steps(self) -> int:
        return int(self.raw["training"]["total_steps"])

    @property
    def learning_rate(self) -> float:
        return float(self.raw["training"]["learning_rate"])

    @property
    def momentum(self) -> float:
        return float(self.raw["training"]["momentum"])

    @property
    def step_pace_ms(self) -> int:
        return int(self.raw.get("runtime", {}).get("step_pace_ms", 0))

    @property
    def registration_timeout_s(self) -> float:
        return float(self.raw.get("network", {}).get("registration_timeout_s", 60.0))

    @property
    def step_timeout_s(self) -> float:
        return float(self.raw.get("network", {}).get("step_timeout_s", 5.0))

    @property
    def abort_timeout_s(self) -> float:
        return float(self.raw.get("network", {}).get("abort_timeout_s", 2.0))

    @property
    def max_frame_bytes(self) -> int:
        return int(self.raw.get("network", {}).get("max_frame_bytes", 64 * 1024 * 1024))

    @property
    def checkpoint_steps(self) -> List[int]:
        return sorted({int(v) for v in self.raw["checkpoint"]["steps"]})

    @property
    def checkpoint_bandwidth_mbps(self) -> float:
        return float(self.raw["checkpoint"]["bandwidth_mbps"])

    @property
    def checkpoint_padding_bytes(self) -> int:
        return int(self.raw["checkpoint"].get("padding_bytes", 0))

    @property
    def checkpoint_max_pending(self) -> int:
        return int(self.raw["checkpoint"].get("max_pending", 2))

    @property
    def checkpoint_dir(self) -> Path:
        return self.run_dir / "checkpoints"

    @property
    def fault(self) -> FaultConfig:
        cfg = self.raw.get("fault", {})
        return FaultConfig(
            enabled=bool(cfg.get("enabled", False)),
            shard_id=int(cfg.get("shard_id", 0)),
            after_step=int(cfg.get("after_step", self.total_steps // 2)),
            outage_steps=max(1, int(cfg.get("outage_steps", 4))),
            stall_tolerance_steps=max(1, int(cfg.get("stall_tolerance_steps", 40))),
            replacement_id=str(cfg.get("replacement_id", "worker-recovery")),
            replacement_delay_ms=max(0, int(cfg.get("replacement_delay_ms", 100))),
        )

    @property
    def config_fingerprint(self) -> str:
        stable = copy.deepcopy(self.raw)
        stable.setdefault("run", {})["run_dir"] = "<run_dir>"
        stable.setdefault("run", {})["port"] = "<port>"
        return sha256_bytes(canonical_json_bytes(stable))

    def worker_command(self, worker_id: str, shard_id: int, standby_file: str | None = None) -> List[str]:
        import sys

        command = [
            sys.executable,
            "-m",
            "mlp_ps.worker",
            "--config",
            str(self.source_path),
            "--worker-id",
            worker_id,
            "--shard-id",
            str(shard_id),
        ]
        if standby_file is not None:
            command.extend(["--standby-file", standby_file])
        return command


def _require(mapping: Mapping[str, Any], key: str, parent: str = "config") -> Any:
    if key not in mapping:
        raise BusinessError(E_CONFIG, f"missing configuration field: {parent}.{key}")
    return mapping[key]


def validate_config(cfg: Config) -> None:
    raw = cfg.raw
    for section in ("run", "model", "training", "checkpoint"):
        if section not in raw or not isinstance(raw[section], dict):
            raise BusinessError(E_CONFIG, f"missing configuration section: {section}")

    _require(raw["run"], "seed", "run")
    _require(raw["run"], "host", "run")
    _require(raw["run"], "port", "run")
    _require(raw["model"], "input_dim", "model")
    _require(raw["model"], "hidden_dim", "model")
    _require(raw["model"], "output_dim", "model")
    _require(raw["training"], "sample_count", "training")
    _require(raw["training"], "batch_size", "training")
    _require(raw["training"], "total_steps", "training")
    _require(raw["training"], "learning_rate", "training")
    _require(raw["training"], "momentum", "training")
    _require(raw["checkpoint"], "steps", "checkpoint")
    _require(raw["checkpoint"], "bandwidth_mbps", "checkpoint")

    if not (1 <= cfg.port <= 65535):
        raise BusinessError(E_CONFIG, "run.port must be in [1, 65535]")
    if cfg.input_dim <= 0 or cfg.hidden_dim < SHARD_COUNT or cfg.output_dim < SHARD_COUNT:
        raise BusinessError(
            E_CONFIG,
            "model dimensions must be positive and hidden/output dimensions must be at least 4",
        )
    if cfg.batch_size <= 0 or cfg.batch_size % SHARD_COUNT != 0:
        raise BusinessError(E_CONFIG, "training.batch_size must be divisible by 4")
    if cfg.total_steps <= 0:
        raise BusinessError(E_CONFIG, "training.total_steps must be positive")
    if cfg.learning_rate <= 0:
        raise BusinessError(E_CONFIG, "training.learning_rate must be positive")
    if not (0.0 <= cfg.momentum < 1.0):
        raise BusinessError(E_CONFIG, "training.momentum must be in [0, 1)")
    if cfg.checkpoint_bandwidth_mbps <= 0:
        raise BusinessError(E_CONFIG, "checkpoint.bandwidth_mbps must be positive")
    if cfg.checkpoint_padding_bytes < 0:
        raise BusinessError(E_CONFIG, "checkpoint.padding_bytes must be non-negative")

    required_samples = cfg.total_steps * cfg.batch_size
    if cfg.sample_count < required_samples:
        raise BusinessError(
            E_PROGRESS_RANGE,
            "training sample budget is smaller than total_steps * batch_size",
            {
                "sample_count": cfg.sample_count,
                "required_samples": required_samples,
            },
        )
    if cfg.eval_samples <= 0:
        raise BusinessError(E_CONFIG, "training.eval_samples must be positive")

    for cp_step in cfg.checkpoint_steps:
        if cp_step < 0 or cp_step > cfg.total_steps:
            raise BusinessError(
                E_CONFIG,
                "checkpoint step is outside the training range",
                {"step": cp_step},
            )
    if 0 not in cfg.checkpoint_steps:
        raise BusinessError(E_CONFIG, "checkpoint.steps must include step 0 for recovery anchoring")

    fault = cfg.fault
    if fault.enabled:
        if not (0 <= fault.shard_id < SHARD_COUNT):
            raise BusinessError(E_CONFIG, "fault.shard_id must be in [0, 3]")
        if not (1 <= fault.after_step < cfg.total_steps):
            raise BusinessError(E_CONFIG, "fault.after_step must be in [1, total_steps-1]")
        if fault.stall_tolerance_steps <= fault.outage_steps:
            raise BusinessError(
                E_CONFIG,
                "fault.stall_tolerance_steps must be greater than fault.outage_steps",
            )


def load_config(path: str | Path, run_dir_override: str | Path | None = None) -> Config:
    source_path = Path(path).resolve()
    try:
        raw = json.loads(source_path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise BusinessError(E_CONFIG, f"configuration file not found: {source_path}") from exc
    except json.JSONDecodeError as exc:
        raise BusinessError(E_CONFIG, f"invalid JSON configuration: {exc}") from exc
    if not isinstance(raw, dict):
        raise BusinessError(E_CONFIG, "configuration root must be an object")

    run_dir_value = run_dir_override
    if run_dir_value is None:
        run_dir_value = raw.get("run", {}).get("run_dir", "artifacts/run")
    run_dir = Path(run_dir_value)
    if not run_dir.is_absolute():
        run_dir = (Path.cwd() / run_dir).resolve()
    else:
        run_dir = run_dir.resolve()

    cfg = Config(raw=raw, source_path=source_path, run_dir=run_dir)
    validate_config(cfg)
    return cfg
