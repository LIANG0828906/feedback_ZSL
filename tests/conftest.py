from __future__ import annotations

import pytest

from mocap_workbench.samples import SampleSpec, generate_presets, generate_sample


@pytest.fixture(scope="session")
def samples(tmp_path_factory: pytest.TempPathFactory) -> dict[str, dict]:
    root = tmp_path_factory.mktemp("samples")
    records = generate_presets(root)
    return {record["name"]: record for record in records}


@pytest.fixture(scope="session")
def compact_samples(tmp_path_factory: pytest.TempPathFactory) -> dict[str, dict]:
    root = tmp_path_factory.mktemp("compact-samples")
    specs = [
        SampleSpec(
            name="small",
            seed=7001,
            frames=48,
            joints=16,
            missing_rate=0.08,
            conflict="none",
        ),
        SampleSpec(
            name="large",
            seed=7002,
            frames=240,
            joints=24,
            missing_rate=0.18,
            conflict="none",
        ),
        SampleSpec(
            name="conflict",
            seed=7003,
            frames=60,
            joints=18,
            missing_rate=0.10,
            conflict="bone_length",
        ),
        SampleSpec(
            name="unreachable",
            seed=7004,
            frames=60,
            joints=18,
            missing_rate=0.10,
            conflict="unreachable",
        ),
    ]
    records = {spec.name: generate_sample(root, spec) for spec in specs}
    records["_root"] = {"name": "_root", "path": str(root)}
    return records

