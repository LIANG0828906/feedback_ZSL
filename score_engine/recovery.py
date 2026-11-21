from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .errors import recovery_invalid
from .model import Score, SolverParameters
from .publish import VersionedPublisher
from .solver import PerformanceSolver, TempoMap, note_performance_hash
from .util import (
    atomic_write_json,
    canonical_json_bytes,
    sha256_bytes,
    sha256_json,
)
from .validation import validate_score


RECOVERY_SCHEMA = "score-recovery-v1"
GENESIS_CHAIN_HASH = sha256_bytes(b"GENESIS")


class SimulatedCrash(RuntimeError):
    pass


@dataclass(frozen=True)
class RecoveryCheckpoint:
    path: Path
    part_index: int
    part_id: str
    prior_chain_hash: str
    payload_hash: str
    chain_hash: str
    part: dict[str, Any]


@dataclass
class RecoveryResult:
    sequence: dict[str, Any]
    resumed_part_ids: tuple[str, ...]
    newly_solved_part_ids: tuple[str, ...]
    recovered_note_ids: tuple[str, ...]
    newly_solved_note_ids: tuple[str, ...]
    chain_tip: str
    checkpoints: tuple[RecoveryCheckpoint, ...]

    def report(self) -> dict[str, Any]:
        return {
            "code": "OK",
            "resumed_part_ids": list(self.resumed_part_ids),
            "newly_solved_part_ids": list(self.newly_solved_part_ids),
            "recovered_note_count": len(self.recovered_note_ids),
            "newly_solved_note_count": len(self.newly_solved_note_ids),
            "recovered_note_ids": list(self.recovered_note_ids),
            "newly_solved_note_ids": list(self.newly_solved_note_ids),
            "chain_tip": self.chain_tip,
            "checkpoint_count": len(self.checkpoints),
            "content_hash": self.sequence.get("content_hash"),
        }


class RecoveryManager:
    def __init__(
        self,
        directory: str | Path,
        score: Score,
        *,
        parameter_version: int,
        sequence_version: int = 1,
        parameters: SolverParameters | None = None,
        solver: PerformanceSolver | None = None,
    ) -> None:
        validate_score(score)
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.score = score
        self.parameter_version = parameter_version
        self.sequence_version = sequence_version
        self.parameters = parameters or SolverParameters()
        self.solver = solver or PerformanceSolver(self.parameters)

    def checkpoint_path(self, part_index: int) -> Path:
        return self.directory / f"part-{part_index:06d}.json"

    def _payload(
        self,
        part_index: int,
        part_id: str,
        part: dict[str, Any],
    ) -> dict[str, Any]:
        return {
            "score_hash": self.score.score_hash(),
            "parameter_version": self.parameter_version,
            "sequence_version": self.sequence_version,
            "parameters": self.parameters.to_dict(),
            "part_index": part_index,
            "part_id": part_id,
            "part": part,
        }

    def commit_part(self, part_index: int, part_record: dict[str, Any], prior_chain_hash: str) -> RecoveryCheckpoint:
        part_id = part_record["part_id"]
        payload = self._payload(part_index, part_id, part_record)
        payload_hash = sha256_json(payload)
        chain_hash = sha256_json(
            {"prior_chain_hash": prior_chain_hash, "payload_hash": payload_hash}
        )
        document = {
            "schema": RECOVERY_SCHEMA,
            **payload,
            "prior_chain_hash": prior_chain_hash,
            "payload_hash": payload_hash,
            "chain_hash": chain_hash,
        }
        path = self.checkpoint_path(part_index)
        atomic_write_json(path, document)
        return RecoveryCheckpoint(
            path=path,
            part_index=part_index,
            part_id=part_id,
            prior_chain_hash=prior_chain_hash,
            payload_hash=payload_hash,
            chain_hash=chain_hash,
            part=part_record,
        )

    def _invalid(
        self,
        message: str,
        *,
        path: Path,
        field: str | None = None,
        part_id: str | None = None,
        note_id: str | None = None,
    ) -> Exception:
        return recovery_invalid(
            message,
            checkpoint=str(path),
            field=field,
            part_id=part_id,
            note_id=note_id,
            details=[
                {
                    "checkpoint": str(path),
                    "field": field,
                    "part_id": part_id,
                    "note_id": note_id,
                }
            ],
        )

    def _validate_checkpoint(
        self,
        path: Path,
        part_index: int,
        expected_part_id: str,
        expected_prior_hash: str,
    ) -> RecoveryCheckpoint:
        try:
            with path.open("r", encoding="utf-8") as handle:
                document = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise self._invalid(
                f"恢复快照无法读取或 JSON 损坏: {exc}",
                path=path,
                field="file",
            ) from exc
        if not isinstance(document, dict):
            raise self._invalid("恢复快照根节点必须是对象", path=path, field="root")
        required = [
            "schema",
            "score_hash",
            "parameter_version",
            "sequence_version",
            "parameters",
            "part_index",
            "part_id",
            "part",
            "prior_chain_hash",
            "payload_hash",
            "chain_hash",
        ]
        for field in required:
            if field not in document:
                raise self._invalid(
                    f"恢复快照缺少字段 {field}",
                    path=path,
                    field=field,
                    part_id=expected_part_id,
                )
        if document["schema"] != RECOVERY_SCHEMA:
            raise self._invalid("恢复快照 schema 不匹配", path=path, field="schema")
        if document["score_hash"] != self.score.score_hash():
            raise self._invalid("恢复快照的乐谱哈希不匹配", path=path, field="score_hash")
        if document["parameter_version"] != self.parameter_version:
            raise self._invalid(
                "恢复快照的参数版本不匹配",
                path=path,
                field="parameter_version",
            )
        if document["sequence_version"] != self.sequence_version:
            raise self._invalid(
                "恢复快照的序列版本不匹配",
                path=path,
                field="sequence_version",
            )
        if document["parameters"] != self.parameters.to_dict():
            raise self._invalid("恢复快照的求解参数不匹配", path=path, field="parameters")
        if document["part_index"] != part_index:
            raise self._invalid("恢复快照的声部索引不匹配", path=path, field="part_index")
        if document["part_id"] != expected_part_id:
            raise self._invalid("恢复快照的声部编号不匹配", path=path, field="part_id")
        if document["prior_chain_hash"] != expected_prior_hash:
            raise self._invalid("恢复哈希链在声部边界断链", path=path, field="prior_chain_hash")
        part = document["part"]
        if not isinstance(part, dict) or "notes" not in part:
            raise self._invalid("恢复快照缺少 part.notes", path=path, field="part.notes")
        expected_note_ids = [
            note.id for note in self.score.parts[part_index].notes
        ]
        recovered_note_ids = [note.get("note_id") for note in part["notes"] if isinstance(note, dict)]
        if recovered_note_ids != expected_note_ids:
            first_mismatch = next(
                (
                    pair[0] if pair[0] != pair[1] else pair[0]
                    for pair in zip(expected_note_ids, recovered_note_ids)
                    if pair[0] != pair[1]
                ),
                None,
            )
            if first_mismatch is None and len(expected_note_ids) != len(recovered_note_ids):
                index = min(len(expected_note_ids), len(recovered_note_ids))
                first_mismatch = expected_note_ids[index] if index < len(expected_note_ids) else None
            raise self._invalid(
                "恢复快照中的音符集合不完整或顺序错误",
                path=path,
                field="part.notes",
                part_id=expected_part_id,
                note_id=first_mismatch,
            )
        for note in part["notes"]:
            for field in ("note_id", "pitch", "start_sec", "end_sec", "velocity", "performance_hash"):
                if field not in note:
                    raise self._invalid(
                        f"恢复音符缺少字段 {field}",
                        path=path,
                        field=f"part.notes.{field}",
                        part_id=expected_part_id,
                        note_id=note.get("note_id"),
                    )
            if note["performance_hash"] != note_performance_hash(note):
                raise self._invalid(
                    "恢复音符的表现哈希不匹配",
                    path=path,
                    field="performance_hash",
                    part_id=expected_part_id,
                    note_id=note.get("note_id"),
                )
        payload = self._payload(part_index, expected_part_id, part)
        computed_payload_hash = sha256_json(payload)
        if document["payload_hash"] != computed_payload_hash:
            raise self._invalid(
                "恢复快照 payload 哈希不匹配",
                path=path,
                field="payload_hash",
                part_id=expected_part_id,
            )
        computed_chain_hash = sha256_json(
            {
                "prior_chain_hash": expected_prior_hash,
                "payload_hash": computed_payload_hash,
            }
        )
        if document["chain_hash"] != computed_chain_hash:
            raise self._invalid(
                "恢复哈希链校验失败",
                path=path,
                field="chain_hash",
                part_id=expected_part_id,
            )
        return RecoveryCheckpoint(
            path=path,
            part_index=part_index,
            part_id=expected_part_id,
            prior_chain_hash=expected_prior_hash,
            payload_hash=computed_payload_hash,
            chain_hash=computed_chain_hash,
            part=part,
        )

    def inspect(self) -> tuple[RecoveryCheckpoint, ...]:
        checkpoints: list[RecoveryCheckpoint] = []
        prior_hash = GENESIS_CHAIN_HASH
        for index, part in enumerate(self.score.parts):
            path = self.checkpoint_path(index)
            if not path.exists():
                later = [
                    candidate
                    for candidate in self.directory.glob("part-*.json")
                    if candidate.name > path.name
                ]
                if later:
                    raise recovery_invalid(
                        "恢复哈希链中间缺失快照",
                        checkpoint=str(path),
                        field="file",
                        part_id=part.id,
                        details=[
                            {
                                "checkpoint": str(path),
                                "field": "file",
                                "part_id": part.id,
                                "later_checkpoints": [str(candidate) for candidate in sorted(later)],
                            }
                        ],
                    )
                break
            checkpoint = self._validate_checkpoint(path, index, part.id, prior_hash)
            checkpoints.append(checkpoint)
            prior_hash = checkpoint.chain_hash
        return tuple(checkpoints)

    def resume(
        self,
        *,
        crash_after_new_parts: int | None = None,
        cancel_check: Callable[[], bool] | None = None,
    ) -> RecoveryResult:
        checkpoints = self.inspect()
        precomputed = {checkpoint.part_id: checkpoint.part for checkpoint in checkpoints}
        part_records_by_id = dict(precomputed)
        prior_hash = checkpoints[-1].chain_hash if checkpoints else GENESIS_CHAIN_HASH
        resumed_part_ids = [checkpoint.part_id for checkpoint in checkpoints]
        recovered_note_ids = [
            note["note_id"]
            for checkpoint in checkpoints
            for note in checkpoint.part["notes"]
        ]
        newly_solved_ids: list[str] = []
        newly_solved_note_ids: list[str] = []
        tempo_map = TempoMap(self.score)
        from .solver import ExpressionCurve

        expression_curve = ExpressionCurve(self.score)
        for index, part in enumerate(self.score.parts):
            if part.id in precomputed:
                continue
            part_record = self.solver.solve_part(
                self.score,
                part,
                tempo_map=tempo_map,
                expression_curve=expression_curve,
                cancel_check=cancel_check,
            )
            checkpoint = self.commit_part(index, part_record, prior_hash)
            checkpoints = checkpoints + (checkpoint,)
            prior_hash = checkpoint.chain_hash
            part_records_by_id[part.id] = part_record
            newly_solved_ids.append(part.id)
            newly_solved_note_ids.extend(note.id for note in part.notes)
            if crash_after_new_parts is not None and len(newly_solved_ids) >= crash_after_new_parts:
                raise SimulatedCrash(
                    f"模拟强制终止：已提交 {len(newly_solved_ids)} 个新声部"
                )
        part_records = [part_records_by_id[part.id] for part in self.score.parts]
        sequence = self.solver.assemble(self.score, part_records, parameters=self.parameters)
        return RecoveryResult(
            sequence=sequence,
            resumed_part_ids=tuple(resumed_part_ids),
            newly_solved_part_ids=tuple(newly_solved_ids),
            recovered_note_ids=tuple(recovered_note_ids),
            newly_solved_note_ids=tuple(newly_solved_note_ids),
            chain_tip=prior_hash,
            checkpoints=checkpoints,
        )

    def commit_and_publish(
        self,
        publisher: VersionedPublisher,
        *,
        version: int | None = None,
        crash_after_new_parts: int | None = None,
    ) -> RecoveryResult:
        result = self.resume(crash_after_new_parts=crash_after_new_parts)
        publisher.publish(result.sequence, version=version)
        return result

