"""Observable block-domain loudness diagnostics for 5.1 PCM material."""

from .batch import run_batch
from .blocks import (
    BlockSpec,
    analyze_asset,
    compare_reports,
    compare_slicing,
    evaluate_block_specs,
    finalize_report,
    make_block_evaluator,
    slice_blocks,
)
from .dsp import (
    K_WEIGHTING_COEFFICIENTS,
    energy_to_loudness,
    k_weight,
    loudness_to_energy,
    oversampled_peak_envelope,
    true_peak_4x,
    weighted_downmix,
)
from .errors import (
    E_BUSY,
    E_CONFIG_INVALID,
    E_HEADER_INVALID,
    E_RECOVERY_INVALID,
    E_SNAPSHOT_RELEASED,
    E_VERSION_MISMATCH,
    DiagnosticError,
)
from .gate import GateParameters, apply_gate, reconstruct_gate_decisions
from .scheduler import BlockScheduler, SchedulerConfig, load_scheduler_config
from .snapshot import SnapshotStore, snapshot_parameter_version
from .version_store import VersionedStore
from .wav import (
    CHANNEL_LAYOUTS,
    CHANNEL_WEIGHTS,
    WavData,
    WavInfo,
    read_wav,
    read_wav_bytes,
)

__all__ = [
    "CHANNEL_LAYOUTS",
    "CHANNEL_WEIGHTS",
    "E_BUSY",
    "E_CONFIG_INVALID",
    "E_HEADER_INVALID",
    "E_RECOVERY_INVALID",
    "E_SNAPSHOT_RELEASED",
    "E_VERSION_MISMATCH",
    "K_WEIGHTING_COEFFICIENTS",
    "BlockScheduler",
    "BlockSpec",
    "DiagnosticError",
    "GateParameters",
    "SchedulerConfig",
    "SnapshotStore",
    "VersionedStore",
    "WavData",
    "WavInfo",
    "analyze_asset",
    "apply_gate",
    "compare_reports",
    "compare_slicing",
    "energy_to_loudness",
    "evaluate_block_specs",
    "finalize_report",
    "k_weight",
    "load_scheduler_config",
    "loudness_to_energy",
    "make_block_evaluator",
    "oversampled_peak_envelope",
    "read_wav",
    "read_wav_bytes",
    "reconstruct_gate_decisions",
    "run_batch",
    "slice_blocks",
    "snapshot_parameter_version",
    "true_peak_4x",
    "weighted_downmix",
]

__version__ = "1.0.0"

