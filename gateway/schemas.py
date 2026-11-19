"""Loose request schemas so validation stays inside the gateway layer."""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel, Field


class RankRequest(BaseModel):
    request_id: str
    tenant_id: str
    client_id: str | None = None
    protocol_version: str
    columns: list[Any] = Field(default_factory=list)
    features: list[Any] = Field(default_factory=list)


class WeightRequest(BaseModel):
    tenant_id: str | None = None
    weight: float
    reset_circuit_breaker: bool = False


class ReloadRequest(BaseModel):
    version: str


class SlowCallRequest(BaseModel):
    version: str
    count: int
    delay_ms: float
