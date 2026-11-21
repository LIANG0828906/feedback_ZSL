"""Regenerate the checked-in v1/v2/v3 interoperability examples."""

from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mcsync.models import (
    CompensationConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
)
from mcsync.pipeline import SyncPipeline
from mcsync.synth import SyntheticConfig, generate_synthetic_audio
from mcsync.versioning import save_report


def main() -> int:
    output = ROOT / "examples"
    output.mkdir(parents=True, exist_ok=True)
    synthetic = generate_synthetic_audio(
        SyntheticConfig(
            channels=2,
            duration_seconds=3.0,
            marker_period_seconds=0.5,
            drift_ppm=(0.0, 90.0),
            initial_offset_samples=(0.0, 3.0),
            seed=20261122,
        )
    )
    result = SyncPipeline(
        PipelineConfig(
            marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
            segmentation=SegmentationConfig(segment_length_samples=48000),
            compensation=CompensationConfig(
                mode="linear", tolerance_samples=2.0
            ),
        )
    ).analyze(synthetic.audio)
    save_report(result.report, output / "sync_report_v1.json", "v1")
    save_report(result.report, output / "sync_report_v2.json", "v2")
    save_report(result.report, output / "sync_report_v3.json", "v3")
    v1 = save_report(result.report, output / "marker_description_v1.json", "v1")
    del v1
    from mcsync.atomic import atomic_write_json

    atomic_write_json(output / "baseline_residual.json", synthetic.baseline)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

