"""Manifest loading, metadata checks, and input validation."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .codes import E_CHAN, E_CLIP, E_IO, E_META, E_ORDER, E_PARA
from .errors import WorkbenchError, error
from .models import (
    ChannelLayout,
    InjectionConfig,
    Sample,
    array_digest,
    canonical_json,
)

CLIP_CEILING = 1.0


class SampleRepository:
    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.manifest_path = self.root / "manifest.json"
        self.manifest = self._load_manifest()

    def _load_manifest(self) -> dict[str, Any]:
        try:
            data = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except FileNotFoundError as exc:
            raise error(
                E_IO,
                f"manifest not found: {self.manifest_path}",
                stage="io.manifest",
                context={"path": str(self.manifest_path)},
            ) from exc
        except (OSError, json.JSONDecodeError) as exc:
            raise error(
                E_IO,
                f"could not read manifest: {exc}",
                stage="io.manifest",
                context={"path": str(self.manifest_path)},
            ) from exc
        if not isinstance(data, dict) or "samples" not in data:
            raise error(
                E_META,
                "manifest must contain a samples array",
                stage="io.manifest",
                field="samples",
            )
        return data

    def suites(self) -> list[dict[str, Any]]:
        return [dict(item) for item in self.manifest.get("suites", [])]

    def sample_entries(self, suite: str | None = None) -> list[dict[str, Any]]:
        items = self.manifest.get("samples", [])
        if suite:
            items = [item for item in items if item.get("suite") == suite]
        return [dict(item) for item in items]

    def sample_ids(self, suite: str | None = None) -> list[str]:
        return [str(item["id"]) for item in self.sample_entries(suite)]

    def injection_cases(self) -> list[dict[str, Any]]:
        return [dict(item) for item in self.manifest.get("injection_cases", [])]

    def _entry(self, sample_id: str) -> dict[str, Any]:
        for entry in self.manifest.get("samples", []):
            if entry.get("id") == sample_id:
                return dict(entry)
        raise error(
            E_META,
            f"unknown sample id: {sample_id}",
            stage="io.lookup",
            field="sample_id",
            context={"sample_id": sample_id},
        )

    def _resolve_path(self, relative: str) -> Path:
        candidate = (self.root / relative).resolve()
        try:
            candidate.relative_to(self.root.resolve())
        except ValueError as exc:
            raise error(
                E_IO,
                "sample path escapes the samples root",
                stage="io.path",
                context={"path": relative},
            ) from exc
        return candidate

    def load(
        self,
        sample_id: str,
        *,
        injection: InjectionConfig | None = None,
        validate: bool = True,
    ) -> Sample:
        entry = self._entry(sample_id)
        sample_rate = entry.get("sample_rate")
        layout_data = entry.get("layout")
        if sample_rate is None:
            raise error(
                E_META,
                "sample metadata is missing sample_rate",
                stage="io.load",
                field="sample_rate",
                context={"sample_id": sample_id},
            )
        if not layout_data or not layout_data.get("channels"):
            raise error(
                E_META,
                "sample metadata is missing channel layout",
                stage="io.load",
                field="layout",
                context={"sample_id": sample_id},
            )
        try:
            sample_rate_int = int(sample_rate)
            layout = ChannelLayout.from_dict(layout_data)
        except (KeyError, TypeError, ValueError) as exc:
            raise error(
                E_META,
                f"invalid sample metadata: {exc}",
                stage="io.load",
                field="layout",
                context={"sample_id": sample_id},
            ) from exc
        if sample_rate_int <= 0:
            raise error(
                E_PARA,
                "sample_rate must be positive",
                stage="io.load",
                field="sample_rate",
                context={"sample_id": sample_id},
            )

        ir_path = self._resolve_path(str(entry.get("ir_path", "")))
        dry_path = self._resolve_path(str(entry.get("dry_path", "")))
        try:
            ir = np.load(ir_path, allow_pickle=False)
            dry = np.load(dry_path, allow_pickle=False)
        except (OSError, ValueError) as exc:
            raise error(
                E_IO,
                f"could not load sample arrays: {exc}",
                stage="io.arrays",
                context={"ir_path": str(ir_path), "dry_path": str(dry_path)},
            ) from exc

        if ir.ndim != 2:
            raise error(
                E_CHAN,
                "IR payload must be a channels-by-samples matrix",
                stage="io.shape",
                field="ir",
                context={"shape": list(ir.shape)},
            )
        if dry.ndim != 1:
            raise error(
                E_META,
                "dry payload must be a mono sample vector",
                stage="io.shape",
                field="dry",
                context={"shape": list(dry.shape)},
            )

        active_injection = injection or InjectionConfig()
        if active_injection.missing_channel:
            if active_injection.missing_channel not in layout.channel_ids:
                raise error(
                    E_CHAN,
                    "injection references an unknown channel",
                    stage="io.injection",
                    channel=active_injection.missing_channel,
                )
            channel_index = layout.channel_ids.index(active_injection.missing_channel)
            ir = np.delete(ir, channel_index, axis=0)
        if active_injection.gain_scale != 1.0:
            ir = ir.astype(np.float32, copy=True)
            ir *= np.float32(active_injection.gain_scale)

        source_hash = array_digest((ir, dry))
        sample = Sample(
            id=sample_id,
            suite=str(entry.get("suite", "")),
            sample_rate=sample_rate_int,
            layout=layout,
            ir=ir,
            dry=dry,
            ir_path=ir_path,
            dry_path=dry_path,
            truth=dict(entry.get("truth", {})),
            injection=active_injection,
            source_hash=source_hash,
        )
        if validate:
            try:
                validate_sample(sample)
            except WorkbenchError as exc:
                exc.add_stage(
                    "io.load",
                    context={
                        "sample_id": sample_id,
                        "injection": active_injection.to_dict(),
                    },
                )
                raise
        return sample


def validate_sample(sample: Sample) -> None:
    if sample.sample_rate is None or sample.sample_rate <= 0:
        raise error(
            E_META,
            "sample rate is missing or invalid",
            stage="validation.metadata",
            field="sample_rate",
            context={"sample_id": sample.id},
        )
    if sample.layout is None or not sample.layout.channels:
        raise error(
            E_META,
            "channel layout is missing",
            stage="validation.metadata",
            field="layout",
            context={"sample_id": sample.id},
        )
    if sample.ir.ndim != 2:
        raise error(
            E_CHAN,
            "IR channel matrix is malformed",
            stage="validation.channels",
            field="ir",
            context={"shape": list(sample.ir.shape)},
        )
    if sample.ir.shape[0] != len(sample.layout.channels):
        raise error(
            E_CHAN,
            "IR channel count does not match the declared layout",
            stage="validation.channels",
            field="layout.channels",
            context={
                "declared": len(sample.layout.channels),
                "actual": int(sample.ir.shape[0]),
            },
        )
    if sample.ir.shape[1] == 0 or sample.dry.size == 0:
        raise error(
            E_META,
            "IR and dry payloads must be non-empty",
            stage="validation.shape",
            field="ir",
        )
    if not np.all(np.isfinite(sample.ir)) or not np.all(np.isfinite(sample.dry)):
        raise error(
            E_CLIP,
            "input contains non-finite samples",
            stage="validation.numeric",
        )
    peak = float(np.max(np.abs(sample.ir)))
    if peak > CLIP_CEILING + 1e-7:
        raise error(
            E_CLIP,
            "IR exceeds the digital full-scale boundary",
            stage="validation.clip",
            field="ir",
            context={"peak": peak, "ceiling": CLIP_CEILING},
        )
    if sample.injection.invert_timing_order:
        raise error(
            E_ORDER,
            "channel timing order is inverted relative to the declared layout",
            stage="validation.order",
            field="timing_order",
            context={"layout": list(sample.layout.channel_ids)},
        )


def load_injection_case(repository: SampleRepository, case_id: str) -> tuple[Sample, dict[str, Any]]:
    for item in repository.injection_cases():
        if item.get("id") == case_id:
            injection = InjectionConfig.from_dict(item.get("injection"))
            return repository.load(str(item["sample_id"]), injection=injection), item
    raise error(
        E_META,
        f"unknown injection case: {case_id}",
        stage="io.injection_lookup",
        field="case_id",
        context={"case_id": case_id},
    )


def manifest_digest(manifest: dict[str, Any]) -> str:
    import hashlib

    return hashlib.sha256(canonical_json(manifest).encode("utf-8")).hexdigest()
