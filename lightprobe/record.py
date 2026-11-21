from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Any

import numpy as np

from .util import canonical_json, sha256_array, sha256_bytes, sha256_json


@dataclass(frozen=True)
class ProbeRecord:
    probe_id: str
    coordinate_hash: str
    raw_radiance_coefficients: np.ndarray
    coefficients: np.ndarray
    visibility_fraction: float
    leak_score: float
    truncation_error: float
    audit_mean_radiance: tuple[float, float, float]
    raw_mean_radiance: tuple[float, float, float]
    audit_sky_mean: tuple[float, float, float]
    first_hit_ids: tuple[int, ...]
    first_hit_distances: tuple[float, ...]
    source_ids: tuple[str, ...]
    comp_source_ids: tuple[str, ...]
    sampled: bool
    payload_hash: str = ""
    influence_version: str = ""
    content_hash: str = ""

    def compute_payload_hash(self) -> str:
        payload = {
            "probe_id": self.probe_id,
            "coordinate_hash": self.coordinate_hash,
            "raw_radiance_hash": sha256_array(self.raw_radiance_coefficients),
            "coefficient_hash": sha256_array(self.coefficients),
            "visibility_fraction": round(float(self.visibility_fraction), 12),
            "leak_score": round(float(self.leak_score), 12),
            "truncation_error": round(float(self.truncation_error), 12),
            "audit_mean_radiance": [round(float(item), 12) for item in self.audit_mean_radiance],
            "raw_mean_radiance": [round(float(item), 12) for item in self.raw_mean_radiance],
            "audit_sky_mean": [round(float(item), 12) for item in self.audit_sky_mean],
            "first_hit_ids": list(self.first_hit_ids),
            "first_hit_distances": [
                round(float(item), 9) if np.isfinite(item) else None
                for item in self.first_hit_distances
            ],
            "source_ids": list(self.source_ids),
            "comp_source_ids": list(self.comp_source_ids),
            "sampled": self.sampled,
        }
        return sha256_json(payload)

    def with_hashes(self, influence_version: str) -> "ProbeRecord":
        payload_hash = self.compute_payload_hash()
        content_hash = sha256_bytes(
            canonical_json(
                {"payload_hash": payload_hash, "influence_version": influence_version}
            )
        )
        return replace(
            self,
            payload_hash=payload_hash,
            influence_version=influence_version,
            content_hash=content_hash,
        )

    def verify_payload_hash(self) -> bool:
        return self.compute_payload_hash() == self.payload_hash

    def verify_content_hash(self) -> bool:
        if not self.verify_payload_hash():
            return False
        expected = sha256_bytes(
            canonical_json(
                {
                    "payload_hash": self.payload_hash,
                    "influence_version": self.influence_version,
                }
            )
        )
        return expected == self.content_hash

    def to_dict(self) -> dict[str, Any]:
        return {
            "probe_id": self.probe_id,
            "coordinate_hash": self.coordinate_hash,
            "raw_radiance_coefficients": self.raw_radiance_coefficients.tolist(),
            "coefficients": self.coefficients.tolist(),
            "visibility_fraction": float(self.visibility_fraction),
            "leak_score": float(self.leak_score),
            "truncation_error": float(self.truncation_error),
            "audit_mean_radiance": list(self.audit_mean_radiance),
            "raw_mean_radiance": list(self.raw_mean_radiance),
            "audit_sky_mean": list(self.audit_sky_mean),
            "first_hit_ids": list(self.first_hit_ids),
            "first_hit_distances": [
                None if not np.isfinite(item) else float(item) for item in self.first_hit_distances
            ],
            "source_ids": list(self.source_ids),
            "comp_source_ids": list(self.comp_source_ids),
            "sampled": bool(self.sampled),
            "payload_hash": self.payload_hash,
            "influence_version": self.influence_version,
            "content_hash": self.content_hash,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "ProbeRecord":
        distances = tuple(
            float("inf") if item is None else float(item)
            for item in payload["first_hit_distances"]
        )
        raw = np.asarray(payload["raw_radiance_coefficients"], dtype=np.float64)
        coefficients = np.asarray(payload["coefficients"], dtype=np.float64)
        raw.setflags(write=False)
        coefficients.setflags(write=False)
        return cls(
            probe_id=str(payload["probe_id"]),
            coordinate_hash=str(payload["coordinate_hash"]),
            raw_radiance_coefficients=raw,
            coefficients=coefficients,
            visibility_fraction=float(payload["visibility_fraction"]),
            leak_score=float(payload["leak_score"]),
            truncation_error=float(payload["truncation_error"]),
            audit_mean_radiance=tuple(float(item) for item in payload["audit_mean_radiance"]),
            raw_mean_radiance=tuple(float(item) for item in payload["raw_mean_radiance"]),
            audit_sky_mean=tuple(float(item) for item in payload["audit_sky_mean"]),
            first_hit_ids=tuple(int(item) for item in payload["first_hit_ids"]),
            first_hit_distances=distances,
            source_ids=tuple(str(item) for item in payload["source_ids"]),
            comp_source_ids=tuple(str(item) for item in payload["comp_source_ids"]),
            sampled=bool(payload["sampled"]),
            payload_hash=str(payload["payload_hash"]),
            influence_version=str(payload["influence_version"]),
            content_hash=str(payload["content_hash"]),
        )

