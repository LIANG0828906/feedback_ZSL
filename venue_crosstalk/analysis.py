"""Pair-level and band-level coupling analysis."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .config import AnalysisConfig, BandSpec
from .dsp import BandEstimate, estimate_band_transfer
from .errors import E_MEASUREMENT_INVALID, VenueError
from .geometry import AlignedMeasurement, align_measurement, resolve_distance
from .models import PairSpec, Workspace
from .utils import ensure_within, sha256_file, sha256_json
from .wavio import read_wav_channel


@dataclass(frozen=True)
class LoadedPair:
    pair: PairSpec
    reference: Any
    pickup: Any
    pickup_wav_hash: str
    reference_wav_hash: str
    content_hash: str

    def summary(self) -> dict[str, Any]:
        return {
            "pair": self.pair.pair_id,
            "source": self.pair.source,
            "pickup": self.pair.pickup,
            "layout": self.pair.layout,
            "pickup_wav_hash": self.pickup_wav_hash,
            "reference_wav_hash": self.reference_wav_hash,
            "content_hash": self.content_hash,
        }


def load_pair_measurement(workspace: Workspace, pair: PairSpec) -> LoadedPair:
    wav_path = ensure_within(workspace.root, pair.wav_path)
    try:
        pickup_wav_hash = sha256_file(wav_path)
    except OSError as exc:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            f"cannot hash measurement WAV: {exc}",
            {"pair": pair.pair_id, "path": str(wav_path)},
        ) from exc
    pickup = read_wav_channel(
        wav_path,
        pair.pickup_channel_index,
        expected_rate=workspace.config.sample_rate,
        expected_layout=pair.layout,
    )
    if pair.source_channel_index is not None:
        reference = read_wav_channel(
            wav_path,
            pair.source_channel_index,
            expected_rate=workspace.config.sample_rate,
            expected_layout=pair.layout,
        )
        reference_path = wav_path
        reference_hash = pickup_wav_hash
    else:
        if not pair.reference_path:
            raise VenueError(
                E_MEASUREMENT_INVALID,
                "mono measurement requires a reference WAV path",
                {"pair": pair.pair_id, "path": str(wav_path)},
            )
        reference_path = ensure_within(workspace.root, pair.reference_path)
        try:
            reference_hash = sha256_file(reference_path)
        except OSError as exc:
            raise VenueError(
                E_MEASUREMENT_INVALID,
                f"cannot hash reference WAV: {exc}",
                {"pair": pair.pair_id, "path": str(reference_path)},
            ) from exc
        reference = read_wav_channel(
            reference_path,
            pair.reference_channel_index,
            expected_rate=workspace.config.sample_rate,
        )
    if reference.size != pickup.size:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            "reference and pickup WAV frame counts differ",
            {
                "pair": pair.pair_id,
                "reference_frames": int(reference.size),
                "pickup_frames": int(pickup.size),
            },
        )
    content_hash = sha256_json(
        {
            "pair": pair.to_dict(),
            "pickup_hash": pickup_wav_hash,
            "reference_hash": reference_hash,
        }
    )
    return LoadedPair(
        pair=pair,
        reference=reference,
        pickup=pickup,
        pickup_wav_hash=pickup_wav_hash,
        reference_wav_hash=reference_hash,
        content_hash=content_hash,
    )


def align_loaded_pair(
    workspace: Workspace,
    loaded: LoadedPair,
) -> AlignedMeasurement:
    return align_measurement(
        loaded.pair,
        workspace.geometry,
        loaded.reference,
        loaded.pickup,
        workspace.config.sample_rate,
    )


def analyze_band_coupling(
    pair_id: str,
    reference: Any,
    pickup: Any,
    band: BandSpec,
    config: AnalysisConfig,
) -> BandEstimate:
    estimate = estimate_band_transfer(pair_id, reference, pickup, band, config)
    content_hash = sha256_json(
        {
            "pair": pair_id,
            "band": band.to_dict(),
            "encoder_version": config.estimator_version,
            "magnitude": estimate.magnitude,
            "phase": estimate.phase_rad,
            "magnitude_uncertainty_db": estimate.magnitude_uncertainty_db,
            "phase_uncertainty_deg": estimate.phase_uncertainty_deg,
            "coherence": estimate.coherence,
            "sample_count": estimate.sample_count,
            "transition_hz": estimate.transition_hz,
        }
    )
    return BandEstimate(
        pair_id=estimate.pair_id,
        band=estimate.band,
        magnitude=estimate.magnitude,
        magnitude_db=estimate.magnitude_db,
        phase_rad=estimate.phase_rad,
        phase_deg=estimate.phase_deg,
        magnitude_uncertainty_db=estimate.magnitude_uncertainty_db,
        phase_uncertainty_deg=estimate.phase_uncertainty_deg,
        magnitude_low=estimate.magnitude_low,
        magnitude_high=estimate.magnitude_high,
        phase_low_deg=estimate.phase_low_deg,
        phase_high_deg=estimate.phase_high_deg,
        coherence=estimate.coherence,
        sample_count=estimate.sample_count,
        effective_samples=estimate.effective_samples,
        quantization_step=estimate.quantization_step,
        transition_hz=estimate.transition_hz,
        content_hash=content_hash,
    )


def estimate_all_bands(
    loaded: LoadedPair,
    aligned: AlignedMeasurement,
    config: AnalysisConfig,
) -> list[BandEstimate]:
    return [
        analyze_band_coupling(
            loaded.pair.pair_id,
            aligned.reference,
            aligned.pickup,
            band,
            config,
        )
        for band in config.bands
    ]
