"""Per-field error upper-bound propagation and factor attribution."""

from __future__ import annotations

from dataclasses import dataclass, field
from decimal import Decimal
from typing import Any

from .errors import E_ATTR_UNRESOLVED, fail
from .model import decimal_string, float_string
from .numeric import half_ulp


POSITION_FACTORS = (
    "param.velocity.quantization",
    "param.heading.quantization",
    "trig.approximation",
    "accum.velocity.quantization",
    "accum.position.quantization",
    "threshold.flip",
)
RESOURCE_FACTORS = (
    "param.resource_delta.quantization",
    "accum.resource.quantization",
    "threshold.resource",
)


@dataclass
class ErrorComponent:
    factor: str
    step: int
    op_id: str
    x: Decimal = Decimal(0)
    y: Decimal = Decimal(0)
    resource: Decimal = Decimal(0)

    def to_payload(self) -> dict[str, Any]:
        return {
            "factor": self.factor,
            "step": self.step,
            "op_id": self.op_id,
            "x": decimal_string(self.x),
            "y": decimal_string(self.y),
            "resource": decimal_string(self.resource),
        }

    @classmethod
    def from_payload(cls, payload: dict[str, Any]) -> "ErrorComponent":
        return cls(
            factor=str(payload["factor"]),
            step=int(payload["step"]),
            op_id=str(payload.get("op_id", "")),
            x=Decimal(str(payload.get("x", "0"))),
            y=Decimal(str(payload.get("y", "0"))),
            resource=Decimal(str(payload.get("resource", "0"))),
        )


@dataclass
class ErrorTracker:
    param_places: int
    accum_places: int
    flip_threshold: Decimal
    resource_threshold: Decimal
    bounds: dict[str, dict[str, Decimal]] = field(default_factory=dict)
    components: dict[str, list[ErrorComponent]] = field(default_factory=dict)
    resource_bound: Decimal = Decimal(0)
    resource_components: list[ErrorComponent] = field(default_factory=list)
    untrusted: dict[int, list[dict[str, Any]]] = field(default_factory=dict)

    def ensure_entity(self, entity_id: str) -> None:
        if entity_id not in self.bounds:
            self.bounds[entity_id] = {
                "x": Decimal(0),
                "y": Decimal(0),
                "vx": Decimal(0),
                "vy": Decimal(0),
                "heading": Decimal(0),
            }
            self.components[entity_id] = []

    def add_fixed_operation(
        self,
        *,
        entity_id: str,
        op_id: str,
        step: int,
        speed: Decimal,
        heading: Decimal,
        dt: Decimal,
        trig_error: Decimal,
    ) -> None:
        self.ensure_entity(entity_id)
        eps_param = half_ulp(self.param_places)
        eps_accum = half_ulp(self.accum_places)

        velocity_speed = eps_param
        velocity_heading = speed * eps_param
        velocity_trig = speed * trig_error
        velocity_accum = eps_accum
        position_accum = eps_accum

        # Conservative axis-independent derivative bounds.  trig errors and
        # heading quantization are bounded by |d(cos)|,|d(sin)| <= 1.
        component_values = (
            ("param.velocity.quantization", velocity_speed * dt),
            ("param.heading.quantization", velocity_heading * dt),
            ("trig.approximation", velocity_trig * dt),
            ("accum.velocity.quantization", velocity_accum * dt),
            ("accum.position.quantization", position_accum),
        )
        for factor, amount in component_values:
            component = ErrorComponent(
                factor=factor,
                step=step,
                op_id=op_id,
                x=amount,
                y=amount,
            )
            self.components[entity_id].append(component)
            bound = self.bounds[entity_id]
            bound["x"] += amount
            bound["y"] += amount

        bound = self.bounds[entity_id]
        bound["vx"] += velocity_speed + velocity_heading + velocity_trig + velocity_accum
        bound["vy"] += velocity_speed + velocity_heading + velocity_trig + velocity_accum
        bound["heading"] += eps_param

    def add_fixed_resource(self, *, op_id: str, step: int) -> None:
        eps_param = half_ulp(self.param_places)
        eps_accum = half_ulp(self.accum_places)
        for factor, amount in (
            ("param.resource_delta.quantization", eps_param),
            ("accum.resource.quantization", eps_accum),
        ):
            component = ErrorComponent(
                factor=factor,
                step=step,
                op_id=op_id,
                resource=amount,
            )
            self.resource_components.append(component)
            self.resource_bound += amount

    def _attribute(
        self,
        components: list[ErrorComponent],
        *,
        axis: str,
        gap: Decimal,
        step: int,
        field_path: str,
        candidates: list[str],
    ) -> dict[str, Any]:
        cumulative = Decimal(0)
        for component in components:
            amount = component.resource if axis == "resource" else getattr(component, axis)
            cumulative += amount
            if cumulative >= gap:
                return {
                    "factor": component.factor,
                    "step": component.step,
                    "op_id": component.op_id,
                    "cumulative_error": decimal_string(cumulative),
                    "threshold_gap": decimal_string(gap),
                    "field": field_path,
                }
        fail(
            E_ATTR_UNRESOLVED,
            "error bound crossed a decision threshold but no factor could be attributed",
            field_path,
            step=step,
            details={
                "candidates": candidates,
                "threshold_gap": decimal_string(gap),
                "components_seen": [component.factor for component in components],
            },
        )
        raise AssertionError("unreachable")

    def evaluate_step(
        self,
        *,
        step: int,
        entities: dict[str, Any],
        room_resource: Decimal,
    ) -> dict[str, Any]:
        step_untrusted: list[dict[str, Any]] = []
        for entity_id in sorted(entities):
            self.ensure_entity(entity_id)
            entity = entities[entity_id]
            x = (
                Decimal(float_string(entity.x))
                if isinstance(entity.x, float)
                else Decimal(str(entity.x))
            )
            y = (
                Decimal(float_string(entity.y))
                if isinstance(entity.y, float)
                else Decimal(str(entity.y))
            )
            bound = self.bounds[entity_id]
            axis = "x" if abs(x) >= abs(y) else "y"
            base = abs(x) if axis == "x" else abs(y)
            uncertainty = bound[axis]
            gap = self.flip_threshold - base
            if gap > 0 and uncertainty >= gap:
                attribution = self._attribute(
                    self.components[entity_id],
                    axis=axis,
                    gap=gap,
                    step=step,
                    field_path=f"entities.{entity_id}.position.{axis}",
                    candidates=list(POSITION_FACTORS),
                )
                entry = {
                    "step": step,
                    "scope": "position",
                    "field": f"entities.{entity_id}.position.{axis}",
                    "value": decimal_string(base),
                    "error_upper_bound": decimal_string(uncertainty),
                    "threshold": decimal_string(self.flip_threshold),
                    "attribution": attribution,
                }
                step_untrusted.append(entry)

        if self.resource_threshold >= 0 and self.resource_bound > self.resource_threshold:
            gap = self.resource_threshold
            attribution = self._attribute(
                self.resource_components,
                axis="resource",
                gap=gap,
                step=step,
                field_path="room.resource",
                candidates=list(RESOURCE_FACTORS),
            )
            step_untrusted.append(
                {
                    "step": step,
                    "scope": "resource",
                    "field": "room.resource",
                    "value": decimal_string(room_resource),
                    "error_upper_bound": decimal_string(self.resource_bound),
                    "threshold": decimal_string(self.resource_threshold),
                    "attribution": attribution,
                }
            )

        if step_untrusted:
            self.untrusted[step] = step_untrusted
        return {"untrusted": step_untrusted}

    def summary(self, entities: dict[str, Any]) -> dict[str, Any]:
        result: dict[str, Any] = {"entities": {}, "room": {}}
        for entity_id in sorted(entities):
            self.ensure_entity(entity_id)
            bound = self.bounds[entity_id]
            result["entities"][entity_id] = {
                "position": {
                    "x": decimal_string(bound["x"]),
                    "y": decimal_string(bound["y"]),
                },
                "velocity": {
                    "x": decimal_string(bound["vx"]),
                    "y": decimal_string(bound["vy"]),
                },
                "heading": decimal_string(bound["heading"]),
            }
        result["room"]["resource"] = decimal_string(self.resource_bound)
        return result

    def snapshot_state(self) -> dict[str, Any]:
        return {
            "param_places": self.param_places,
            "accum_places": self.accum_places,
            "flip_threshold": decimal_string(self.flip_threshold),
            "resource_threshold": decimal_string(self.resource_threshold),
            "bounds": {
                entity_id: {
                    name: decimal_string(value)
                    for name, value in sorted(bounds.items())
                }
                for entity_id, bounds in sorted(self.bounds.items())
            },
            "components": {
                entity_id: [component.to_payload() for component in components]
                for entity_id, components in sorted(self.components.items())
            },
            "resource_bound": decimal_string(self.resource_bound),
            "resource_components": [
                component.to_payload() for component in self.resource_components
            ],
        }

    @classmethod
    def from_snapshot_state(cls, payload: dict[str, Any]) -> "ErrorTracker":
        tracker = cls(
            param_places=int(payload["param_places"]),
            accum_places=int(payload["accum_places"]),
            flip_threshold=Decimal(str(payload["flip_threshold"])),
            resource_threshold=Decimal(str(payload["resource_threshold"])),
        )
        for entity_id, bounds in payload.get("bounds", {}).items():
            tracker.bounds[str(entity_id)] = {
                name: Decimal(str(value)) for name, value in bounds.items()
            }
        for entity_id, components in payload.get("components", {}).items():
            tracker.components[str(entity_id)] = [
                ErrorComponent.from_payload(component) for component in components
            ]
        tracker.resource_bound = Decimal(str(payload.get("resource_bound", "0")))
        tracker.resource_components = [
            ErrorComponent.from_payload(component)
            for component in payload.get("resource_components", [])
        ]
        return tracker
