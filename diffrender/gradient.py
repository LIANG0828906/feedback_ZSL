"""Gradient norm clipping, explosion detection and per-parameter freezing."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

import torch

from .errors import AppError, fail
from .params import SceneParameterLayout


@dataclass(frozen=True)
class GuardConfig:
    clip_enabled: bool = True
    clip_norm: float = 5.0
    explode_threshold: float = 1.0e4
    failure_patience: int = 2

    def validate(self) -> None:
        if not isinstance(self.clip_enabled, bool):
            fail("E_PARA", "clip_enabled must be boolean", "clip_enabled")
        for name in ("clip_norm", "explode_threshold"):
            value = float(getattr(self, name))
            if not math.isfinite(value) or value <= 0.0:
                fail(
                    "E_PARA",
                    f"{name} must be a positive finite number",
                    name,
                    value=value,
                )
        if (
            isinstance(self.failure_patience, bool)
            or not isinstance(self.failure_patience, int)
            or self.failure_patience <= 0
        ):
            fail(
                "E_PARA",
                "failure_patience must be a positive integer",
                "failure_patience",
            )

    def fingerprint_payload(self) -> dict[str, Any]:
        return {
            "clip_enabled": self.clip_enabled,
            "clip_norm": self.clip_norm,
            "explode_threshold": self.explode_threshold,
            "failure_patience": self.failure_patience,
        }


@dataclass
class GuardReport:
    iteration: int
    global_norm_preclip: float
    global_norm_applied: float
    clipping_triggered: bool
    parameter_norms: dict[str, float]
    frozen_parameters: list[str]
    newly_failed: list[str]
    invalid_nonfinite_parameters: list[str]
    suspect_parameters: list[str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "iteration": self.iteration,
            "global_norm_preclip": self.global_norm_preclip,
            "global_norm_applied": self.global_norm_applied,
            "clipping_triggered": self.clipping_triggered,
            "parameter_norms": self.parameter_norms,
            "frozen_parameters": self.frozen_parameters,
            "newly_failed": self.newly_failed,
            "invalid_nonfinite_parameters": self.invalid_nonfinite_parameters,
            "suspect_parameters": self.suspect_parameters,
        }


class GradientGuard:
    def __init__(
        self,
        layout: SceneParameterLayout,
        config: GuardConfig,
    ) -> None:
        config.validate()
        self.layout = layout
        self.config = config
        self.failure_counts: dict[str, int] = {
            label: 0 for label in layout.labels
        }
        self.frozen: set[str] = set()
        self.clip_events_total = 0
        self.history: list[dict[str, Any]] = []

    def process(
        self,
        raw: torch.Tensor,
        *,
        iteration: int,
        gradient_spikes: dict[str, float] | None = None,
    ) -> GuardReport:
        if raw.grad is None:
            fail("E_GRAD", "parameter gradient is missing", "gradient")
        gradient = raw.grad
        if gradient.shape != raw.shape:
            fail(
                "E_GRAD",
                "parameter gradient shape does not match parameters",
                "gradient",
                expected=list(raw.shape),
                actual=list(gradient.shape),
            )
        spikes = gradient_spikes or {}
        unknown = sorted(set(spikes) - set(self.layout.label_to_index))
        if unknown:
            fail(
                "E_PARA",
                "gradient spike injection references unknown parameters",
                "gradient_spikes",
                unknown=unknown,
            )
        for label, scale in spikes.items():
            value = float(scale)
            if not math.isfinite(value) or value <= 0.0:
                fail(
                    "E_PARA",
                    "gradient spike scale must be positive and finite",
                    f"gradient_spikes.{label}",
                )
            gradient[self.layout.label_to_index[label]] *= value

        invalid_nonfinite: list[str] = []
        norms: dict[str, float] = {}
        for label in self.layout.labels:
            index = self.layout.label_to_index[label]
            value = float(gradient[index].detach().abs().item())
            norms[label] = value
            if not math.isfinite(value):
                invalid_nonfinite.append(label)
                self.failure_counts[label] = self.config.failure_patience
                gradient[index] = 0.0
        global_preclip = float(
            torch.sqrt(torch.sum(gradient.detach().square())).item()
        )

        newly_failed: list[str] = []
        suspect: set[str] = set()
        for label in self.layout.labels:
            index = self.layout.label_to_index[label]
            if label in self.frozen:
                gradient[index] = 0.0
                continue
            if label in invalid_nonfinite or norms[label] > self.config.explode_threshold:
                self.failure_counts[label] += 1
                suspect.add(label)
            else:
                self.failure_counts[label] = 0
            if self.failure_counts[label] >= self.config.failure_patience:
                self.frozen.add(label)
                newly_failed.append(label)
                gradient[index] = 0.0

        for label in suspect:
            gradient[self.layout.label_to_index[label]] = 0.0
        for label in self.frozen:
            gradient[self.layout.label_to_index[label]] = 0.0

        applied_norm = float(
            torch.sqrt(torch.sum(gradient.detach().square())).item()
        )
        clipping_triggered = False
        if self.config.clip_enabled and applied_norm > self.config.clip_norm:
            scale = self.config.clip_norm / (applied_norm + 1.0e-12)
            gradient.mul_(scale)
            applied_norm = self.config.clip_norm
            clipping_triggered = True
            self.clip_events_total += 1

        report = GuardReport(
            iteration=iteration,
            global_norm_preclip=global_preclip,
            global_norm_applied=applied_norm,
            clipping_triggered=clipping_triggered,
            parameter_norms=norms,
            frozen_parameters=sorted(self.frozen),
            newly_failed=sorted(newly_failed),
            invalid_nonfinite_parameters=sorted(invalid_nonfinite),
            suspect_parameters=sorted(suspect),
        )
        self.history.append(report.to_dict())
        return report

    def raise_if_failed(self, report: GuardReport) -> None:
        if report.newly_failed or report.invalid_nonfinite_parameters:
            raise AppError(
                "E_GRAD",
                "gradient failure threshold reached; affected parameters were frozen",
                "gradient",
                {
                    "iteration": report.iteration,
                    "frozen_parameters": report.frozen_parameters,
                    "newly_failed": report.newly_failed,
                    "invalid_nonfinite_parameters": report.invalid_nonfinite_parameters,
                    "other_parameters_updated": True,
                },
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "failure_counts": dict(sorted(self.failure_counts.items())),
            "frozen": sorted(self.frozen),
            "clip_events_total": self.clip_events_total,
            "history": self.history,
        }

    @classmethod
    def from_dict(
        cls,
        layout: SceneParameterLayout,
        config: GuardConfig,
        payload: dict[str, Any],
    ) -> "GradientGuard":
        guard = cls(layout, config)
        incoming_counts = payload.get("failure_counts", {})
        for label in layout.labels:
            guard.failure_counts[label] = int(incoming_counts.get(label, 0))
        guard.frozen = set(str(v) for v in payload.get("frozen", []))
        unknown = sorted(guard.frozen - set(layout.labels))
        if unknown:
            fail(
                "E_CHECKPOINT",
                "gradient checkpoint contains unknown frozen parameters",
                "guard.frozen",
                unknown=unknown,
            )
        guard.clip_events_total = int(payload.get("clip_events_total", 0))
        guard.history = list(payload.get("history", []))
        return guard

