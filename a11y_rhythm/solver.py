"""Priority-aware reading rhythm solver with deterministic local conflict chains."""

from __future__ import annotations

import copy
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable

from .errors import E_CANCELED, E_DESC_CONFLICT, E_RECOVERY_INVALID, RhythmError
from .models import (
    ConflictRecord,
    ItemResult,
    SolveResult,
    SourceBundle,
    SourceItem,
    content_hash,
    fraction_display_text,
)
from .timebase import FrameTable


CancelCheck = Callable[[], bool]
ProgressCallback = Callable[[dict[str, Any]], None]


@dataclass
class ImpactPlan:
    affected_ids: list[str]
    reasons: dict[str, str]


@dataclass
class _WorkItem:
    source: SourceItem
    start: int
    end: int
    minimum: int
    active: bool = True
    decision: str = "initial"
    readable: bool = True
    conflicts: list[dict[str, Any]] = field(default_factory=list)
    depends_on: list[str] = field(default_factory=list)

    @property
    def item_id(self) -> str:
        return self.source.item_id

    @property
    def kind(self) -> str:
        return self.source.kind

    @property
    def duration(self) -> int:
        return max(0, self.end - self.start)

    @property
    def minimum_duration(self) -> int:
        return self.minimum

    def to_checkpoint(self) -> dict[str, Any]:
        return {
            "start": self.start,
            "end": self.end,
            "active": self.active,
            "decision": self.decision,
            "readable": self.readable,
            "conflicts": copy.deepcopy(self.conflicts),
            "depends_on": list(self.depends_on),
        }

    def load_checkpoint(self, value: dict[str, Any]) -> None:
        self.start = int(value["start"])
        self.end = int(value["end"])
        self.active = bool(value["active"])
        self.decision = str(value["decision"])
        self.readable = bool(value["readable"])
        self.conflicts = copy.deepcopy(value.get("conflicts", []))
        self.depends_on = list(value.get("depends_on", []))


class _ExpandImpact(Exception):
    def __init__(self, item_id: str, reason: str):
        self.item_id = item_id
        self.reason = reason
        super().__init__(reason)


class RhythmSolver:
    """Solve all sentence kinds on one integer sample timeline.

    The resolver is deterministic: item order, tie-breaking, iteration order and
    conflict evidence do not depend on wall-clock time or global random state.
    """

    def __init__(
        self,
        bundle: SourceBundle,
        frame_table: FrameTable,
        coverage_limit: int,
        *,
        cancel_check: CancelCheck | None = None,
        progress: ProgressCallback | None = None,
    ) -> None:
        self.bundle = bundle
        self.frame_table = frame_table
        self.coverage_limit = coverage_limit
        self.cancel_check = cancel_check or (lambda: False)
        self.progress = progress
        self.sample_rate = bundle.sample_rate
        self.sources = {item.item_id: item for item in bundle.items}
        self.order = sorted(
            bundle.items,
            key=lambda item: (item.start_sample, -item.priority, item.item_id),
        )
        self.order_ids = [item.item_id for item in self.order]
        self._iteration = 0
        self._records: list[ConflictRecord] = []
        self._chain: list[str] = []
        self._mutable_ids: set[str] = set()
        self._reasons: dict[str, str] = {}
        self._works: dict[str, _WorkItem] = {}

    # ------------------------------------------------------------------ plan
    def plan_impact(
        self,
        changed_id: str,
        base_results: dict[str, ItemResult] | None,
    ) -> ImpactPlan:
        if not base_results or changed_id not in self.sources:
            return ImpactPlan(list(self.order_ids), {"*": "no reusable baseline; conservative full solve"})

        affected: set[str] = {changed_id}
        reasons: dict[str, str] = {changed_id: "edited sentence"}
        base = base_results

        def add(item_id: str, reason: str) -> bool:
            if item_id not in self.sources or item_id in affected:
                return False
            affected.add(item_id)
            reasons[item_id] = reason
            return True

        # Old conflict edges are symmetric proof that the pair previously
        # competed for the same duration.
        queue = [changed_id]
        while queue:
            current = queue.pop(0)
            previous = base.get(current)
            if previous is None:
                continue
            for conflict in previous.conflicts:
                other = conflict.get("other_id")
                if isinstance(other, str) and add(other, f"prior conflict edge from {current}"):
                    queue.append(other)

        # A changed description can grow to its readability minimum. Speech can
        # consume at most its original span. This is a conservative right reach.
        changed_source = self.sources[changed_id]
        reach = changed_source.start_sample + max(
            changed_source.nominal_duration,
            changed_source.minimum_samples(self.sample_rate),
        )
        for item_id in self.order_ids:
            if item_id in affected:
                reach = max(reach, self._max_reach(item_id, base))

        progress = True
        while progress:
            progress = False
            for item_id in self.order_ids:
                if item_id in affected:
                    continue
                source = self.sources[item_id]
                if source.start_sample < changed_source.start_sample:
                    reasons.setdefault(
                        item_id,
                        f"conservatively reused: starts before edited sentence {changed_id}",
                    )
                    continue
                required_gap = source.min_gap_samples if source.kind == "description" else 0
                if source.start_sample <= reach + required_gap:
                    add(item_id, f"timeline reach {reach}+gap{required_gap} reaches start {source.start_sample}")
                    reach = max(reach, self._max_reach(item_id, base))
                    progress = True
                    continue

                # If the item follows an affected item in the prior dependency
                # graph, it cannot be proven independent.
                previous_result = base.get(item_id)
                if previous_result and any(dep in affected for dep in previous_result.depends_on):
                    add(item_id, "depends on affected predecessor")
                    reach = max(reach, self._max_reach(item_id, base))
                    progress = True
                    continue

                # The first item fully beyond the reach is the proof boundary.
                safe_start = source.start_sample
                if safe_start > reach + required_gap:
                    reasons.setdefault(
                        item_id,
                        f"conservatively reused: start {safe_start} > reach {reach} + gap {required_gap}",
                    )
                    # Later items are not necessarily independent of this one,
                    # but their own start is later; continue scanning so one
                    # unusually close later item can still be included.

        # Stable timeline order in public output.
        affected_order = [item_id for item_id in self.order_ids if item_id in affected]
        return ImpactPlan(affected_order, reasons)

    def _max_reach(self, item_id: str, base: dict[str, ItemResult]) -> int:
        source = self.sources[item_id]
        natural = max(source.nominal_duration, source.minimum_samples(self.sample_rate))
        previous = base.get(item_id)
        if previous is not None and previous.status != "rejected":
            natural = max(natural, previous.end - source.start_sample)
        return source.start_sample + natural

    # ----------------------------------------------------------------- solve
    def solve(
        self,
        *,
        version: int,
        changed_id: str | None = None,
        base_results: dict[str, ItemResult] | None = None,
        checkpoint: dict[str, Any] | None = None,
    ) -> SolveResult:
        base = copy.deepcopy(base_results) if base_results else None
        if checkpoint is not None:
            plan = ImpactPlan(
                list(checkpoint["affected_ids"]),
                dict(checkpoint.get("reasons", {})),
            )
            self._mutable_ids = set(plan.affected_ids)
            self._reasons = plan.reasons
            self._init_works(base, plan.affected_ids)
            self._restore_checkpoint(checkpoint)
        else:
            if changed_id is None or base is None:
                plan = ImpactPlan(list(self.order_ids), {"*": "initial full solve"})
            else:
                plan = self.plan_impact(changed_id, base)
            # Never reuse a cache entry whose sentence fingerprint changed,
            # even when the timeline reach heuristic considered it safe.
            if base is not None:
                for source in self.order:
                    previous = base.get(source.item_id)
                    if (
                        source.item_id not in plan.affected_ids
                        and previous is not None
                        and previous.input_fingerprint
                        and previous.input_fingerprint != source.fingerprint
                    ):
                        plan.affected_ids.append(source.item_id)
                        plan.reasons[source.item_id] = "cached input fingerprint mismatch"
                plan.affected_ids = sorted(set(plan.affected_ids), key=self.order_ids.index)
            self._mutable_ids = set(plan.affected_ids)
            self._reasons = plan.reasons
            self._init_works(base, plan.affected_ids)

        while True:
            try:
                self._resolve()
                self._verify_reused(base)
                break
            except _ExpandImpact as expansion:
                if expansion.item_id in self._mutable_ids:
                    raise RhythmError(
                        E_DESC_CONFLICT,
                        "impact expansion requested for an already mutable item",
                        expansion.item_id,
                        {"reason": expansion.reason, "checkpoint": self.checkpoint()},
                    )
                self._mutable_ids.add(expansion.item_id)
                self._reasons[expansion.item_id] = expansion.reason
                # The previous partial pass may have made decisions while the
                # newly affected sentence was still treated as immutable.
                # Restart the deterministic resolution pass with the expanded
                # set instead of carrying those decisions forward.
                self._iteration = 0
                self._records = []
                self._chain = []
                self._init_works(base, sorted(self._mutable_ids, key=self.order_ids.index))

        results = self._build_results()
        result = SolveResult(
            version=version,
            material_id=self.bundle.material_id,
            params_version=self.bundle.params_version,
            sample_rate=self.sample_rate,
            source_hash=self.bundle.source_hash,
            items=results,
            conflicts=copy.deepcopy(self._records),
            affected_ids=sorted(self._mutable_ids, key=self.order_ids.index),
            recomputed_ids=sorted(self._mutable_ids, key=self.order_ids.index),
            reused_ids=[item_id for item_id in self.order_ids if item_id not in self._mutable_ids],
            conservative_reasons=dict(self._reasons),
            iterations=self._iteration,
            stable=True,
        )
        result.trace_events = self._build_trace(result)
        result.finalize_hash()
        return result

    def _init_works(self, base: dict[str, ItemResult] | None, mutable_ids: Iterable[str]) -> None:
        mutable = set(mutable_ids)
        works: dict[str, _WorkItem] = {}
        for source in self.order:
            if source.item_id not in mutable and base and source.item_id in base:
                previous = base[source.item_id]
                if previous.input_fingerprint and previous.input_fingerprint != source.fingerprint:
                    raise _ExpandImpact(source.item_id, "cached input fingerprint no longer matches")
                work = _WorkItem(
                    source=source,
                    start=previous.start,
                    end=previous.end,
                    minimum=previous.minimum_duration,
                    active=previous.status != "rejected",
                    decision=previous.decision,
                    readable=previous.readable,
                    conflicts=copy.deepcopy(previous.conflicts),
                    depends_on=list(previous.depends_on),
                )
            else:
                minimum = source.minimum_samples(self.sample_rate)
                if source.kind == "description":
                    start = source.start_sample
                    end = max(source.end_sample, start + minimum)
                    end = min(end, self.coverage_limit)
                else:
                    start = source.start_sample
                    end = source.end_sample
                work = _WorkItem(
                    source=source,
                    start=start,
                    end=end,
                    minimum=minimum,
                    active=True,
                    decision="initial",
                    readable=max(0, end - start) >= minimum,
                )
            works[source.item_id] = work
        self._works = works

    def _resolve(self) -> None:
        while True:
            if self.cancel_check():
                raise RhythmError(
                    E_CANCELED,
                    "solve canceled cooperatively",
                    None,
                    {"iteration": self._iteration, "checkpoint": self.checkpoint()},
                )
            conflict = self._first_conflict()
            if conflict is None:
                return
            if self._iteration >= self.bundle.max_iterations:
                evidence = {
                    "iteration": self._iteration,
                    "max_iterations": self.bundle.max_iterations,
                    "unresolved": {
                        "left_id": conflict[0].item_id,
                        "right_id": conflict[1].item_id,
                        "left_interval": [conflict[0].start, conflict[0].end],
                        "right_interval": [conflict[1].start, conflict[1].end],
                    },
                    "conflict_chain": list(self._chain) or [conflict[0].item_id, conflict[1].item_id],
                    "records": [record.to_dict() for record in self._records[-32:]],
                    "checkpoint": self.checkpoint(),
                }
                raise RhythmError(
                    E_DESC_CONFLICT,
                    "description conflict chain did not stabilize within the declared limit",
                    conflict[0].item_id,
                    evidence,
                )
            self._resolve_pair(conflict[0], conflict[1])
            self._emit_progress()

    def _first_conflict(self) -> tuple[_WorkItem, _WorkItem] | None:
        active = [work for work in self._works.values() if work.active]
        active.sort(key=lambda work: (work.start, work.end, -work.source.priority, work.item_id))
        for index, left in enumerate(active):
            for right in active[index + 1 :]:
                gap = self._required_gap(left, right)
                if right.start < left.end + gap:
                    return left, right
        return None

    @staticmethod
    def _required_gap(left: _WorkItem, right: _WorkItem) -> int:
        if left.kind == "description" or right.kind == "description":
            return max(left.source.min_gap_samples, right.source.min_gap_samples)
        return 0

    def _resolve_pair(self, left: _WorkItem, right: _WorkItem) -> None:
        self._iteration += 1
        gap = self._required_gap(left, right)
        self._append_chain(left.item_id, right.item_id)
        left_before = (left.start, left.end)
        right_before = (right.start, right.end)

        if left.kind == "description" and right.kind == "description":
            winner, loser = self._priority_winner(left, right)
            self._require_mutable(loser.item_id, "lower-priority description lost an overlap")
            self._reject(loser, winner, gap)
            action = "reject_description"
        elif left.kind == "description" or right.kind == "description":
            if left.kind == "description":
                desc, speech = left, right
            else:
                desc, speech = right, left
            if speech.source.priority >= desc.source.priority:
                self._require_mutable(desc.item_id, "speech priority won; description must be rejected")
                self._reject(desc, speech, gap)
                action = "reject_description"
                winner, loser = speech, desc
            else:
                self._require_mutable(speech.item_id, "higher-priority description preempted speech")
                self._make_room(speech, desc, gap)
                action = "compress_or_shift_speech"
                winner, loser = desc, speech
        else:
            # Speech order is authoritative. Reordering same-kind sentences by
            # priority can oscillate forever; priority is used when a
            # description competes with speech.
            winner, loser = left, right
            self._require_mutable(loser.item_id, "speech duration competition propagated downstream")
            self._make_room(loser, winner, 0)
            action = "compress_or_shift_speech"

        record = ConflictRecord(
            iteration=self._iteration,
            chain=list(self._chain),
            left_id=left.item_id,
            right_id=right.item_id,
            action=action,
            winner_id=winner.item_id if winner.active else None,
            loser_id=loser.item_id,
            reason=(
                f"{winner.item_id} priority {winner.source.priority} "
                f"resolved against {loser.item_id} priority {loser.source.priority}"
            ),
            left_before=left_before,
            right_before=right_before,
        )
        self._records.append(record)
        evidence = {
            "iteration": self._iteration,
            "action": action,
            "winner_id": record.winner_id,
            "loser_id": record.loser_id,
            "left_interval": list(left_before),
            "right_interval": list(right_before),
        }
        left.conflicts.append({"other_id": right.item_id, **evidence})
        right.conflicts.append({"other_id": left.item_id, **evidence})
        self._refresh_readability(left)
        self._refresh_readability(right)

    @staticmethod
    def _priority_winner(left: _WorkItem, right: _WorkItem) -> tuple[_WorkItem, _WorkItem]:
        if left.source.priority > right.source.priority:
            return left, right
        if right.source.priority > left.source.priority:
            return right, left
        if (left.start, left.item_id) <= (right.start, right.item_id):
            return left, right
        return right, left

    def _require_mutable(self, item_id: str, reason: str) -> None:
        if item_id not in self._mutable_ids:
            raise _ExpandImpact(item_id, f"unproven dependency: {reason}")

    def _reject(self, loser: _WorkItem, winner: _WorkItem, gap: int) -> None:
        loser.active = False
        loser.readable = False
        loser.decision = "rejected"
        loser.depends_on = _append_unique(loser.depends_on, winner.item_id)

    def _make_room(self, loser: _WorkItem, winner: _WorkItem, gap: int) -> None:
        minimum = loser.minimum_duration
        # Preserve what is currently readable. An already-unreadable sentence
        # must not be inflated to its target and consume the rest of the track.
        current_duration = loser.duration if loser.duration > 0 else minimum
        # Trim the overlapped suffix when the winner begins after the loser.
        if winner.start > loser.start:
            candidate_end = max(loser.start, winner.start - gap)
            if candidate_end - loser.start >= minimum:
                old_end = loser.end
                loser.end = candidate_end
                loser.decision = "compressed"
                loser.depends_on = _append_unique(loser.depends_on, winner.item_id)
                if old_end == loser.end:
                    loser.decision = "conflict"
                return

        new_start = winner.end + gap
        target_duration = current_duration
        new_end = new_start + target_duration
        if new_end > self.coverage_limit:
            compressed_end = new_start + minimum
            if compressed_end <= self.coverage_limit:
                loser.end = compressed_end
                loser.start = new_start
                loser.decision = "shifted_compressed"
                loser.depends_on = _append_unique(loser.depends_on, winner.item_id)
                return
            raise RhythmError(
                E_DESC_CONFLICT,
                "conflict chain reached the end of the audio timebase",
                loser.item_id,
                {
                    "winner_id": winner.item_id,
                    "loser_id": loser.item_id,
                    "required_end": compressed_end,
                    "coverage_limit": self.coverage_limit,
                    "conflict_chain": list(self._chain),
                    "records": [record.to_dict() for record in self._records[-32:]],
                    "checkpoint": self.checkpoint(),
                },
            )
        loser.start = new_start
        loser.end = new_end
        loser.decision = "shifted"
        loser.depends_on = _append_unique(loser.depends_on, winner.item_id)

    @staticmethod
    def _refresh_readability(work: _WorkItem) -> None:
        if not work.active:
            work.readable = False
            return
        work.readable = work.duration >= work.minimum_duration
        if not work.readable and work.decision == "initial":
            work.decision = "unreadable"

    def _append_chain(self, *item_ids: str) -> None:
        for item_id in item_ids:
            if not self._chain or self._chain[-1] != item_id:
                self._chain.append(item_id)
        if len(self._chain) > 256:
            self._chain = self._chain[-256:]

    def _verify_reused(self, base: dict[str, ItemResult] | None) -> None:
        if not base:
            return
        for item_id, work in self._works.items():
            if item_id in self._mutable_ids:
                continue
            previous = base.get(item_id)
            if previous is None:
                raise _ExpandImpact(item_id, "reused result is missing")
            if previous.status == "rejected":
                continue
            if previous.start != work.start or previous.end != work.end:
                raise _ExpandImpact(item_id, "reused interval changed after conflict resolution")

    def _build_results(self) -> list[ItemResult]:
        results: list[ItemResult] = []
        for source in self.order:
            work = self._works[source.item_id]
            self._refresh_readability(work)
            frame_start, frame_end = self.frame_table.frame_range(work.start, work.end)
            result = ItemResult(
                item_id=source.item_id,
                kind=source.kind,
                text=source.text,
                speaker=source.speaker,
                priority=source.priority,
                window_start=source.start_sample,
                window_end=source.end_sample,
                start=work.start,
                end=work.end,
                minimum_duration=source.minimum_samples(self.sample_rate),
                readable=work.readable and work.active,
                status="rejected" if not work.active else "active",
                decision=work.decision,
                frame_start=frame_start,
                frame_end=frame_end,
                conflicts=sorted(
                    work.conflicts,
                    key=lambda value: (
                        int(value.get("iteration", 0)),
                        str(value.get("other_id", "")),
                    ),
                ),
                depends_on=sorted(set(work.depends_on)),
                input_fingerprint=source.fingerprint,
                text_length=source.text_length,
                max_reading_rate_cps=fraction_display_text(source.max_cps),
                estimated_reading_rate_cps=fraction_display_text(source.source_rate_cps),
            )
            if result.status == "active" and not result.readable and result.decision == "initial":
                result.decision = "unreadable"
            result.finalize_hash()
            results.append(result)
        return results

    # -------------------------------------------------------------- checkpoints
    def checkpoint(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "source_hash": self.bundle.source_hash,
            "params_version": self.bundle.params_version,
            "iteration": self._iteration,
            "chain": list(self._chain),
            "records": [record.to_dict() for record in self._records],
            "works": {item_id: work.to_checkpoint() for item_id, work in self._works.items()},
            "affected_ids": sorted(self._mutable_ids, key=self.order_ids.index),
            "reasons": dict(self._reasons),
        }

    def _restore_checkpoint(self, value: dict[str, Any]) -> None:
        if not isinstance(value, dict) or value.get("schema_version") != 1:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "checkpoint schema is invalid",
                None,
                {"checkpoint": value},
            )
        if value.get("source_hash") != self.bundle.source_hash:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "checkpoint source hash no longer matches",
                None,
                {
                    "checkpoint_source_hash": value.get("source_hash"),
                    "current_source_hash": self.bundle.source_hash,
                },
            )
        self._iteration = int(value["iteration"])
        self._chain = list(value["chain"])
        self._records = [
            ConflictRecord(
                iteration=int(record["iteration"]),
                chain=list(record["chain"]),
                left_id=record["left_id"],
                right_id=record["right_id"],
                action=record["action"],
                winner_id=record.get("winner_id"),
                loser_id=record.get("loser_id"),
                reason=record["reason"],
                left_before=tuple(record["left_before"]),
                right_before=tuple(record["right_before"]),
            )
            for record in value.get("records", [])
        ]
        self._mutable_ids = set(value["affected_ids"])
        self._reasons = dict(value.get("reasons", {}))
        works = value.get("works", {})
        for item_id, work_value in works.items():
            if item_id in self._works:
                self._works[item_id].load_checkpoint(work_value)

    def _emit_progress(self) -> None:
        if self.progress is not None:
            self.progress(self.checkpoint())

    # ------------------------------------------------------------------- trace
    def _build_trace(self, result: SolveResult) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        affected_set = set(result.affected_ids)
        for sequence, item in enumerate(result.items, start=1):
            if item.item_id not in affected_set and item.item_id not in result.reused_ids:
                continue
            events.append(
                {
                    "event": "sentence_result",
                    "sequence": sequence,
                    "version": result.version,
                    "sentence_id": item.item_id,
                    "affected_count": len(result.affected_ids),
                    "conflict_chain_length": len(
                        {part for record in result.conflicts for part in (record.left_id, record.right_id)}
                    ),
                    "content_hash": content_hash(item.canonical(include_hash=False)),
                    "reused": item.item_id not in affected_set,
                }
            )
        for sequence, record in enumerate(result.conflicts, start=1):
            events.append(
                {
                    "event": "conflict_resolution",
                    "sequence": sequence,
                    "version": result.version,
                    "sentence_id": record.loser_id,
                    "affected_count": len(result.affected_ids),
                    "conflict_chain_length": len(
                        {part for item in result.conflicts for part in (item.left_id, item.right_id)}
                    ),
                    "content_hash": content_hash(record.to_dict()),
                    "action": record.action,
                    "winner_id": record.winner_id,
                }
            )
        return events


def _append_unique(values: list[str], value: str) -> list[str]:
    if value not in values:
        return [*values, value]
    return values
