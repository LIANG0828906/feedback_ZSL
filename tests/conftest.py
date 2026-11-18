from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

import pytest

from rec_gateway.assetgen import generate_assets
from rec_gateway.config import GatewayConfig


@pytest.fixture(scope="session")
def generated_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("generated")
    generate_assets(root, sample_count=800, seed=294)
    return root


@pytest.fixture(scope="session")
def samples(generated_root: Path) -> list[dict[str, Any]]:
    values = []
    with (generated_root / "samples" / "requests.jsonl").open(
        "r", encoding="utf-8"
    ) as handle:
        for line in handle:
            values.append(json.loads(line))
    return values


def make_config(
    tmp_path: Path,
    model_dir: Path,
    weights: dict[str, float] | None = None,
    request_timeout_ms: float = 600.0,
    max_batch_size: int = 64,
) -> GatewayConfig:
    selected_weights = weights or {
        "tenant-a": 0.0,
        "tenant-b": 0.1,
        "tenant-c": 0.5,
        "tenant-d": 1.0,
    }
    return GatewayConfig.from_dict(
        {
            "model_dir": str(model_dir),
            "database": str(tmp_path / "gateway.sqlite3"),
            "max_batch_size": max_batch_size,
            "max_concurrency": 2,
            "request_timeout_ms": request_timeout_ms,
            "fault_injection_enabled": True,
            "tenants": [
                {"tenant_id": tenant_id, "canary_weight": weight}
                for tenant_id, weight in selected_weights.items()
            ],
        },
        base_dir=tmp_path,
    )


def copy_models(source: Path, destination: Path) -> Path:
    shutil.copytree(source, destination)
    return destination


def mutate_manifest(model_dir: Path, version: str, mutator) -> None:
    path = model_dir / version / "manifest.json"
    manifest = json.loads(path.read_text(encoding="utf-8"))
    mutator(manifest)
    path.write_text(
        json.dumps(manifest, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )


def sample_for(
    samples: list[dict[str, Any]],
    protocol_version: str,
    tenant_id: str | None = None,
    client_id: str | None = None,
) -> dict[str, Any]:
    for sample in samples:
        if sample["protocol_version"] != protocol_version:
            continue
        if tenant_id is not None and sample["tenant_id"] != tenant_id:
            continue
        request = dict(sample)
        request["feature_names"] = list(sample["feature_names"])
        request["features"] = list(sample["features"])
        if tenant_id is not None:
            request["tenant_id"] = tenant_id
        if client_id is not None:
            request["client_id"] = client_id
        return request
    raise AssertionError(f"no sample for {protocol_version}")
