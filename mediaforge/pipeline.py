from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .errors import E_IO, E_META, fail
from .injection import InjectionConfig, apply_input_injections
from .metadata import SUPPORTED_VERSIONS, load_metadata
from .model import ContainerModel
from .raw import RawStream, load_raw_directory, merge_raw_streams
from .truth import TruthManifest, load_truth


@dataclass(frozen=True)
class LoadedSample:
    sample_dir: Path
    metadata_version: str
    model: ContainerModel
    truth: TruthManifest | None
    raw_streams: dict[str, RawStream]


def metadata_path(sample_dir: str | Path, version: str = "v1") -> Path:
    return Path(sample_dir) / "metadata" / f"meta_{version}.json"


def truth_path(sample_dir: str | Path) -> Path:
    return Path(sample_dir) / "truth.json"


def raw_path(sample_dir: str | Path) -> Path:
    return Path(sample_dir) / "raw"


def load_sample(
    sample_dir: str | Path,
    metadata_version: str = "v1",
    *,
    injection: InjectionConfig | None = None,
    require_truth: bool = True,
) -> LoadedSample:
    root = Path(sample_dir)
    if metadata_version not in SUPPORTED_VERSIONS:
        fail(E_META, "$.metadata_version", "unsupported sample metadata version", measured=metadata_version)
    meta_file = metadata_path(root, metadata_version)
    truth_file = truth_path(root)
    if not meta_file.exists():
        fail(E_IO, "$.metadata", f"metadata file does not exist: {meta_file}")
    model = load_metadata(meta_file)
    if require_truth and not truth_file.exists():
        fail(E_IO, "$.truth", f"truth manifest does not exist: {truth_file}")
    truth = load_truth(truth_file) if truth_file.exists() else None
    streams = load_raw_directory(raw_path(root))
    model, streams = apply_input_injections(model, streams, injection)
    merged = merge_raw_streams(model, streams, truth=truth)
    return LoadedSample(
        sample_dir=root,
        metadata_version=metadata_version,
        model=merged,
        truth=truth,
        raw_streams=streams,
    )


def discover_sample_groups(samples_root: str | Path) -> list[Path]:
    root = Path(samples_root)
    if not root.exists():
        fail(E_IO, "$.samples", f"samples directory does not exist: {root}")
    groups = sorted(
        path
        for path in root.iterdir()
        if path.is_dir() and (path / "truth.json").exists()
    )
    return groups
