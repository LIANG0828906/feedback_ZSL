from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .models import TierParams


@dataclass(frozen=True)
class DependencyPlan:
    changed_fields: tuple[str, ...]
    direct_dirty: tuple[int, ...]
    full_reuse_prefix: int
    total_slices: int
    phase_changed: bool
    propagation_from: int | None
    reasons: dict[int, str]

    @property
    def reused_slices(self) -> int:
        return self.full_reuse_prefix

    @property
    def recomputed_slices(self) -> int:
        return max(0, self.total_slices - self.full_reuse_prefix)

    def propagation_chain(self, index: int) -> list[int]:
        if self.propagation_from is None or index < self.propagation_from:
            return []
        return list(range(self.propagation_from, index + 1))

    def as_dict(self) -> dict[str, Any]:
        return {
            "changed_fields": list(self.changed_fields),
            "direct_dirty": list(self.direct_dirty),
            "full_reuse_prefix": self.full_reuse_prefix,
            "reused_slices": self.reused_slices,
            "recomputed_slices": self.recomputed_slices,
            "phase_changed": self.phase_changed,
            "propagation_from": self.propagation_from,
            "total_slices": self.total_slices,
            "reasons": {str(index): reason for index, reason in sorted(self.reasons.items())},
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "DependencyPlan":
        return cls(
            changed_fields=tuple(value.get("changed_fields", [])),
            direct_dirty=tuple(int(index) for index in value.get("direct_dirty", [])),
            full_reuse_prefix=int(value.get("full_reuse_prefix", 0)),
            total_slices=int(value.get("total_slices", 0)),
            phase_changed=bool(value.get("phase_changed", False)),
            propagation_from=(
                int(value["propagation_from"])
                if value.get("propagation_from") is not None
                else None
            ),
            reasons={
                int(index): str(reason)
                for index, reason in value.get("reasons", {}).items()
            },
        )


def plan_dependencies(
    old: TierParams | None,
    new: TierParams,
    old_sidecar: dict[str, Any] | None,
) -> DependencyPlan:
    total_slices = (
        int(old_sidecar.get("total_slices", 0)) if old_sidecar else 0
    )
    if old is None or old_sidecar is None:
        return DependencyPlan(
            changed_fields=("initial",),
            direct_dirty=tuple(range(total_slices)),
            full_reuse_prefix=0,
            total_slices=total_slices,
            phase_changed=True,
            propagation_from=0 if total_slices else None,
            reasons={index: "initial" for index in range(total_slices)},
        )

    changed: list[str] = []
    for field in ("target_rate", "filter_length", "quant_bits", "slice_samples"):
        if getattr(old, field) != getattr(new, field):
            changed.append(field)

    if "slice_samples" in changed:
        total_slices = 0
    if total_slices == 0:
        # The caller replaces this with the freshly computed slice count.
        pass

    phase_changed = any(field in changed for field in ("target_rate", "filter_length", "slice_samples"))
    direct_dirty: list[int] = []
    if not changed:
        direct_dirty = []
    elif phase_changed:
        direct_dirty = list(range(total_slices))
    elif changed == ["quant_bits"]:
        old_slices = old_sidecar.get("slices", [])
        direct_dirty = [
            index for index, meta in enumerate(old_slices) if not bool(meta.get("silent"))
        ]
    else:
        direct_dirty = list(range(total_slices))

    earliest = min(direct_dirty) if direct_dirty else None
    full_reuse_prefix = earliest if earliest is not None else total_slices
    reasons: dict[int, str] = {}
    if earliest is not None:
        for index in range(earliest, total_slices):
            if index == earliest:
                reasons[index] = "param:" + ",".join(changed) if changed else "forced"
            elif phase_changed:
                reasons[index] = "phase_propagation"
            else:
                reasons[index] = "hash_chain_propagation"
    return DependencyPlan(
        changed_fields=tuple(changed),
        direct_dirty=tuple(direct_dirty),
        full_reuse_prefix=full_reuse_prefix,
        total_slices=total_slices,
        phase_changed=phase_changed,
        propagation_from=earliest,
        reasons=reasons,
    )


def refresh_plan_total(plan: DependencyPlan, total_slices: int) -> DependencyPlan:
    if plan.total_slices == total_slices:
        return plan
    start = plan.propagation_from
    reasons = dict(plan.reasons)
    if start is not None:
        for index in range(start, total_slices):
            if index not in reasons:
                reasons[index] = (
                    "phase_propagation"
                    if plan.phase_changed
                    else "hash_chain_propagation"
                )
    return DependencyPlan(
        changed_fields=plan.changed_fields,
        direct_dirty=plan.direct_dirty,
        full_reuse_prefix=plan.full_reuse_prefix,
        total_slices=total_slices,
        phase_changed=plan.phase_changed,
        propagation_from=plan.propagation_from,
        reasons=reasons,
    )


def with_safe_prefix(
    plan: DependencyPlan,
    *,
    safe_prefix: int,
    total_slices: int,
) -> DependencyPlan:
    reasons: dict[int, str] = {}
    if safe_prefix < total_slices:
        for index in range(safe_prefix, total_slices):
            if index == safe_prefix:
                reasons[index] = (
                    "param:" + ",".join(plan.changed_fields)
                    if plan.changed_fields
                    else "forced"
                )
            elif plan.phase_changed:
                reasons[index] = "phase_propagation"
            else:
                reasons[index] = "hash_chain_propagation"
    return DependencyPlan(
        changed_fields=plan.changed_fields,
        direct_dirty=tuple(range(safe_prefix, total_slices)),
        full_reuse_prefix=safe_prefix,
        total_slices=total_slices,
        phase_changed=plan.phase_changed,
        propagation_from=safe_prefix if safe_prefix < total_slices else None,
        reasons=reasons,
    )

