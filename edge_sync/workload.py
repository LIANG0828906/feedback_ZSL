"""Deterministic archive workload construction.

Shard sizes are chosen from the configured 4 KiB..64 KiB mix with the fixed
seed; payloads are regenerated on demand by :class:`~edge_sync.rng.ByteStream`
and checksummed, so 12,000 shards need no 180 MiB payload directory.
"""

from __future__ import annotations

from dataclasses import dataclass
import hashlib

from .config import Config
from .rng import ByteStream, uniform


@dataclass(frozen=True)
class ShardSpec:
    task_id: str
    shard_seq: int
    start_byte: int
    end_byte: int
    length: int
    checksum: str


@dataclass(frozen=True)
class TaskSpec:
    task_id: str
    total_bytes: int
    shards: tuple[ShardSpec, ...]

    @property
    def shard_count(self) -> int:
        return len(self.shards)


def _shard_size(config: Config, task_index: int, shard_seq: int) -> int:
    sizes = [size * 1024 for size in config.workload.shard_sizes_kb]
    pick = uniform(config.seed, 71, task_index, shard_seq)
    # Weighted, still deterministic: favor small shards so 12,000 shards stay
    # within the 180 MiB envelope while every configured size still appears.
    weights = (0.30, 0.27, 0.22, 0.15, 0.06)
    cumulative = 0.0
    for size, weight in zip(sizes, weights):
        cumulative += weight
        if pick < cumulative:
            return size
    return sizes[-1]


def build_manifest(config: Config) -> list[TaskSpec]:
    tasks: list[TaskSpec] = []
    for task_index in range(config.workload.tasks):
        task_id = f"task-{task_index + 1:02d}"
        shards: list[ShardSpec] = []
        offset = 0
        for shard_seq in range(config.workload.shards_per_task):
            length = _shard_size(config, task_index, shard_seq)
            stream = ByteStream(config.seed, task_index + 1, shard_seq)
            checksum = hashlib.sha256(stream.render(length)).hexdigest()
            shards.append(
                ShardSpec(
                    task_id=task_id,
                    shard_seq=shard_seq,
                    start_byte=offset,
                    end_byte=offset + length,
                    length=length,
                    checksum=checksum,
                )
            )
            offset += length
        tasks.append(TaskSpec(task_id=task_id, total_bytes=offset, shards=tuple(shards)))
    return tasks


def payload_for(config: Config, shard: ShardSpec) -> bytes:
    task_index = int(shard.task_id.split("-")[1]) - 1
    stream = ByteStream(config.seed, task_index + 1, shard.shard_seq)
    return stream.render(shard.length)


def validate_envelope(config: Config, manifest: list[TaskSpec]) -> list[str]:
    """Enforce the resource/contract limits declared in the task statement."""
    problems: list[str] = []
    if config.total_concurrency > 16:
        problems.append("aggregate shard concurrency exceeds 16")
    total_bytes = sum(task.total_bytes for task in manifest)
    if total_bytes > 180 * 1024 * 1024:
        problems.append(f"workload {total_bytes} bytes exceeds the 180 MiB cap")
    if config.workload.task_concurrency > 4:
        problems.append("per-task concurrency exceeds 4")
    for task in manifest:
        expected_offset = 0
        for shard in task.shards:
            if shard.start_byte != expected_offset or shard.end_byte != expected_offset + shard.length:
                problems.append(f"non-contiguous range {task.task_id}:{shard.shard_seq}")
            expected_offset = shard.end_byte
    return problems
