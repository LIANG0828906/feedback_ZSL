"""Immutable configuration objects and deterministic parameter versions."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass
from typing import Any

from .errors import SciSeqError

ALGORITHM_VERSION = "sciseq-core-1.0.0"

WINDOW_CHOICES = ("none", "hann", "hamming", "blackman")
FILTER_CHOICES = ("gaussian", "median", "guided")


def _para(message: str, field: str, **details: Any) -> SciSeqError:
    return SciSeqError("E_PARA", message, field=field, details=details)


@dataclass(frozen=True)
class RegistrationConfig:
    window: str = "none"
    upsample_factor: int = 20
    max_shift: float = 8.0

    def validate(self, shape: tuple[int, int] | None = None) -> None:
        if self.window not in WINDOW_CHOICES:
            raise _para("unsupported registration window", "registration.window", allowed=WINDOW_CHOICES)
        if not 1 <= int(self.upsample_factor) <= 100:
            raise _para(
                "upsample_factor outside allowed range",
                "registration.upsample_factor",
                allowed=(1, 100),
                actual=self.upsample_factor,
            )
        if not 0.0 < float(self.max_shift) <= 10_000.0:
            raise _para(
                "max_shift outside allowed range",
                "registration.max_shift",
                allowed=(0.0, 10_000.0),
                actual=self.max_shift,
            )
        if shape is not None:
            min_dim = min(shape)
            if self.max_shift > min_dim / 2.0:
                raise _para(
                    "max_shift exceeds half of the smallest frame dimension",
                    "registration.max_shift",
                    allowed=(0.0, min_dim / 2.0),
                    actual=self.max_shift,
                )


@dataclass(frozen=True)
class DenoiseConfig:
    filter: str = "gaussian"
    block_size: int = 32
    strength: float = 1.0
    median_radius: int = 1
    guided_radius: int = 3

    def validate(self, shape: tuple[int, int] | None = None) -> None:
        if self.filter not in FILTER_CHOICES:
            raise _para("unsupported denoise filter", "denoise.filter", allowed=FILTER_CHOICES)
        if not 4 <= int(self.block_size) <= 512:
            raise _para(
                "block_size outside allowed range",
                "denoise.block_size",
                allowed=(4, 512),
                actual=self.block_size,
            )
        if not 0.1 <= float(self.strength) <= 5.0:
            raise _para(
                "strength outside allowed range",
                "denoise.strength",
                allowed=(0.1, 5.0),
                actual=self.strength,
            )
        if not 1 <= int(self.median_radius) <= 3:
            raise _para(
                "median_radius outside allowed range",
                "denoise.median_radius",
                allowed=(1, 3),
                actual=self.median_radius,
            )
        if not 1 <= int(self.guided_radius) <= 16:
            raise _para(
                "guided_radius outside allowed range",
                "denoise.guided_radius",
                allowed=(1, 16),
                actual=self.guided_radius,
            )
        if shape is not None and self.block_size > min(shape):
            raise _para(
                "block_size exceeds frame dimension",
                "denoise.block_size",
                allowed=(4, min(shape)),
                actual=self.block_size,
            )


@dataclass(frozen=True)
class BudgetConfig:
    time_budget_s: float = 120.0
    memory_budget_mb: float = 1024.0
    max_workers: int = 4
    frames_per_worker: int = 2
    preview_denoise_fraction: float = 0.5
    preview_mean_abs_limit: float = 35.0

    def validate(self) -> None:
        if not 0.1 <= float(self.time_budget_s) <= 86_400.0:
            raise _para(
                "time_budget_s outside allowed range",
                "budget.time_budget_s",
                allowed=(0.1, 86_400.0),
                actual=self.time_budget_s,
            )
        if not 16.0 <= float(self.memory_budget_mb) <= 1_000_000.0:
            raise _para(
                "memory_budget_mb outside allowed range",
                "budget.memory_budget_mb",
                allowed=(16.0, 1_000_000.0),
                actual=self.memory_budget_mb,
            )
        if not 1 <= int(self.max_workers) <= 64:
            raise _para(
                "max_workers outside allowed range",
                "budget.max_workers",
                allowed=(1, 64),
                actual=self.max_workers,
            )
        if not 1 <= int(self.frames_per_worker) <= 8:
            raise _para(
                "frames_per_worker outside allowed range",
                "budget.frames_per_worker",
                allowed=(1, 8),
                actual=self.frames_per_worker,
            )
        if not 0.05 <= float(self.preview_denoise_fraction) <= 1.0:
            raise _para(
                "preview_denoise_fraction outside allowed range",
                "budget.preview_denoise_fraction",
                allowed=(0.05, 1.0),
                actual=self.preview_denoise_fraction,
            )
        if not 1.0 <= float(self.preview_mean_abs_limit) <= 255.0:
            raise _para(
                "preview_mean_abs_limit outside allowed range",
                "budget.preview_mean_abs_limit",
                allowed=(1.0, 255.0),
                actual=self.preview_mean_abs_limit,
            )


@dataclass(frozen=True)
class PipelineConfig:
    reference_frame: int = 0
    registration: RegistrationConfig = RegistrationConfig()
    denoise: DenoiseConfig = DenoiseConfig()
    budget: BudgetConfig = BudgetConfig()

    def validate(
        self,
        shape: tuple[int, int] | None = None,
        frame_count: int | None = None,
    ) -> None:
        self.registration.validate(shape)
        self.denoise.validate(shape)
        self.budget.validate()
        if int(self.reference_frame) < 0:
            raise _para(
                "reference_frame must be non-negative",
                "reference_frame",
                allowed=(0, "frame_count - 1"),
                actual=self.reference_frame,
            )
        if frame_count is not None and not 0 <= int(self.reference_frame) < frame_count:
            raise _para(
                "reference_frame outside sequence",
                "reference_frame",
                allowed=(0, frame_count - 1),
                actual=self.reference_frame,
            )

    def output_payload(self) -> dict[str, Any]:
        """Parameters that affect committed pixels and feature statistics."""

        return {
            "reference_frame": int(self.reference_frame),
            "registration": asdict(self.registration),
            "denoise": asdict(self.denoise),
        }

    def parameter_version(self) -> str:
        encoded = json.dumps(
            self.output_payload(),
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()[:24]

    def decision_payload(self) -> dict[str, Any]:
        payload = self.output_payload()
        payload["budget"] = asdict(self.budget)
        return payload

    def decision_version(self) -> str:
        encoded = json.dumps(
            self.decision_payload(),
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()[:24]

