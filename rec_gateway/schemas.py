from __future__ import annotations

import json
from typing import Any

from pydantic import BaseModel, ConfigDict, Field, model_validator


class FaultInjectionConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    inference_delay_ms: float = Field(default=0.0, ge=0.0, le=30000.0)


class InferenceRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    request_id: str | None = Field(default=None, min_length=1, max_length=128)
    tenant_id: str = Field(min_length=1, max_length=128)
    client_id: str | None = Field(default=None, min_length=1, max_length=256)
    protocol_version: str | None = None
    feature_protocol_version: str | None = None
    protocol: str | None = None
    feature_names: list[str] | None = None
    features: Any
    fault_injection: FaultInjectionConfig | None = None

    @model_validator(mode="after")
    def resolve_protocol_version(self) -> "InferenceRequest":
        declared = {
            value
            for value in (
                self.protocol_version,
                self.feature_protocol_version,
                self.protocol,
            )
            if value is not None
        }
        if len(declared) != 1:
            raise ValueError(
                "exactly one of protocol_version, feature_protocol_version, protocol is required"
            )
        self.protocol_version = declared.pop()
        return self

    def canonical_hash(self) -> str:
        payload = {
            "client_id": self.client_id,
            "fault_injection": (
                self.fault_injection.model_dump()
                if self.fault_injection is not None
                else None
            ),
            "feature_names": self.feature_names,
            "features": self.features,
            "protocol_version": self.protocol_version,
            "tenant_id": self.tenant_id,
        }
        import hashlib

        encoded = json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()


class BatchRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    items: list[Any] | None = None
    requests: list[Any] | None = None

    @model_validator(mode="after")
    def require_one_list(self) -> "BatchRequest":
        if (self.items is None) == (self.requests is None):
            raise ValueError("exactly one of items or requests is required")
        return self

    def resolved_items(self) -> list[Any]:
        return self.items if self.items is not None else list(self.requests or [])


class WeightUpdateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    canary_weight: float


class ReloadRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    version: str
