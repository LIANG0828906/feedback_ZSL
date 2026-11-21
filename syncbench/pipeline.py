"""End-to-end synchronization pipeline orchestration."""

from __future__ import annotations

import glob
import hashlib
import json
import os
import threading
from dataclasses import asdict, dataclass

import numpy as np

from .delay import estimate_segment_delays, xcorr_delay
from .diagnose import diagnose
from .errors import E_BUSY, E_CANCELLED, E_CONFIG, SyncError
from .fit import compare_fits, fit_drift, jump_candidates
from .markers import detect_markers
from .report import build_report, save_report
from .resample import MODE_LINEAR, MODE_SINC, resample
from .stitch import SegmentCache, compensate_channel, content_hash, stitch_channels
from .trace import TraceLogger
from .wavio import read_channel_wavs, write_multichannel_wav

STRATEGY_WHOLE = "whole_xcorr"
STRATEGY_SEGMENTED = "segmented_fit"


@dataclass
class SyncConfig:
    seg_len_s: float = 2.0
    ref_channel: int = 0
    mode: str = MODE_LINEAR
    strategy_threshold_s: float = 10.0
    residual_threshold: float = 2.0
    snr_threshold: float = 0.3
    max_lag: int = 2400
    align_tolerance: float = 1.0
    marker_period_s: float = 1.0
    min_seg_conf: float = 5.0

    def validate(self, n_channels):
        if not 0 <= self.ref_channel < n_channels:
            raise SyncError(E_CONFIG, "ref_channel out of range")
        if self.mode not in (MODE_LINEAR, MODE_SINC):
            raise SyncError(E_CONFIG, f"unknown mode {self.mode}")
        if self.seg_len_s <= 0:
            raise SyncError(E_CONFIG, "seg_len_s must be positive")


def _input_hash(signal):
    return hashlib.sha256(
        np.ascontiguousarray(signal, dtype=np.float64).tobytes()
    ).hexdigest()


def _marker_alignment_errors(comp, ref, fs, period_s, snr_threshold):
    """Alignment error of compensated channel markers vs reference markers."""
    det_c = detect_markers(comp, fs, period_s, snr_threshold)["markers"]
    det_r = detect_markers(ref, fs, period_s, snr_threshold)["markers"]
    ref_map = {m["index"]: m["sample"] for m in det_r}
    errors = [
        m["sample"] - ref_map[m["index"]]
        for m in det_c
        if m["index"] in ref_map
    ]
    if not errors:
        return {"n": 0, "max": None, "mean": None, "rms": None}
    arr = np.asarray(errors)
    return {
        "n": int(len(arr)),
        "max": float(np.max(np.abs(arr))),
        "mean": float(np.mean(arr)),
        "rms": float(np.sqrt(np.mean(arr**2))),
    }


class SyncSession:
    """Holds incremental state (cache, previous fits) across runs."""

    def __init__(self):
        self.cache = SegmentCache()
        self.last_fits = {}  # (dataset, channel) -> fit
        self.last_seg_len = {}
        self._lock = threading.Lock()
        self._running = False
        self.cancel_event = threading.Event()
        self.last_result = None

    def try_acquire(self):
        with self._lock:
            if self._running:
                return False
            self._running = True
            return True

    def release(self):
        with self._lock:
            self._running = False
            self.cancel_event.clear()

    def cancel(self):
        self.cancel_event.set()

    def _check_cancel(self):
        if self.cancel_event.is_set():
            raise SyncError(E_CANCELLED, "sync cancelled by user")

    def run(self, data_dir, config: SyncConfig, out_dir, trace_path=None):
        if not self.try_acquire():
            raise SyncError(E_BUSY, "a sync task is already running for this dataset")
        try:
            result = self._run_inner(data_dir, config, out_dir, trace_path)
            self.last_result = result
            return result
        finally:
            self.release()

    def _run_inner(self, data_dir, config, out_dir, trace_path):
        trace = TraceLogger(trace_path)
        paths = sorted(glob.glob(os.path.join(data_dir, "*.wav")))
        if not paths:
            raise SyncError(E_CONFIG, f"no wav files in {data_dir}")
        signals, fs = read_channel_wavs(paths)
        n_ch = len(signals)
        config.validate(n_ch)
        duration_s = min(len(s) for s in signals) / fs
        ds_hash = hashlib.sha256(
            "".join(_input_hash(s) for s in signals).encode()
        ).hexdigest()
        trace.event(
            "LOAD",
            dataset=os.path.basename(data_dir),
            channels=n_ch,
            fs=fs,
            samples=min(len(s) for s in signals),
            dataset_hash=ds_hash[:16],
        )
        self._check_cancel()

        # 1. marker detection per channel
        marker_results = []
        for ch, sig in enumerate(signals):
            det = detect_markers(
                sig, fs, config.marker_period_s, config.snr_threshold
            )
            marker_results.append(det)
            trace.event(
                "MARKERS",
                channel=ch,
                detected=len(det["markers"]),
                unavailable=len(det["unavailable"]),
            )
        self._check_cancel()

        # 2. strategy selection (deterministic cost model, no wall-clock)
        seg_len = int(round(config.seg_len_s * fs))
        n_samples = min(len(s) for s in signals)
        n_segments = n_samples // seg_len
        if duration_s > config.strategy_threshold_s:
            strategy = STRATEGY_SEGMENTED
            reason = "duration_above_threshold"
        else:
            strategy = STRATEGY_WHOLE
            reason = "duration_within_threshold"
        cost_whole = n_samples * int(np.log2(max(n_samples, 2)))
        cost_seg = n_segments * seg_len * int(np.log2(max(seg_len, 2)))
        trace.event(
            "STRATEGY",
            selected=strategy,
            reason=reason,
            segments=n_segments,
            est_cost_whole=cost_whole,
            est_cost_segmented=cost_seg,
        )

        ref = signals[config.ref_channel]
        ref_positions = [
            m["sample"] for m in marker_results[config.ref_channel]["markers"]
        ]

        # 3. delay estimation + drift fit (segmented model always computed;
        #    whole-file model computed for strategy comparison)
        channels_out = []
        for ch, sig in enumerate(signals):
            self._check_cancel()
            if ch == config.ref_channel:
                fit = fit_drift([0.0], [0.0])
                segments = []
                diag = {
                    "conclusion": "REFERENCE",
                    "evidence_segments": [],
                    "residual_stats": {"residual_rms": 0.0},
                }
            else:
                segments = estimate_segment_delays(
                    ref, sig, seg_len, config.max_lag, config.min_seg_conf,
                    ref_markers=ref_positions,
                )
                valid = [s for s in segments if s["valid"]]
                centers = [s["center"] for s in valid]
                delays = [s["delay"] for s in valid]
                fit = fit_drift(centers, delays)
                diag = diagnose(
                    centers, delays, config.residual_threshold,
                    indices=[s["segment"] for s in valid],
                )
            jumps = jump_candidates(fit["residuals"], config.residual_threshold)
            whole_delay, whole_conf = xcorr_delay(ref, sig, config.max_lag)
            trace.event(
                "FIT",
                channel=ch,
                ppm=round(fit["ppm"], 6),
                offset=round(fit["offset"], 6),
                residual_rms=round(fit["residual_rms"], 6),
                jump_candidates=len(jumps),
                diagnosis=diag["conclusion"],
            )
            # fit-diff against previous run with different segment length
            fit_diff = None
            key = (ds_hash, ch)
            prev = self.last_fits.get(key)
            prev_seg = self.last_seg_len.get(key)
            if prev is not None and prev_seg != config.seg_len_s:
                fit_diff = compare_fits(prev, fit)
                trace.event(
                    "FIT_DIFF",
                    channel=ch,
                    delta_ppm=round(fit_diff["delta_ppm"], 6),
                    delta_offset=round(fit_diff["delta_offset"], 6),
                )
            self.last_fits[key] = fit
            self.last_seg_len[key] = config.seg_len_s
            channels_out.append(
                {
                    "channel": ch,
                    "markers": marker_results[ch],
                    "segments": segments,
                    "fit": fit,
                    "jump_candidates": jumps,
                    "diagnosis": diag,
                    "whole_delay": whole_delay,
                    "whole_confidence": whole_conf,
                    "fit_diff": fit_diff,
                }
            )
        self._check_cancel()

        # 4. compensation with incremental cache; both modes compared
        seg_out_len = seg_len
        mode_stats = {}
        compensated_by_mode = {}
        for mode in dict.fromkeys([config.mode, MODE_LINEAR, MODE_SINC]):
            comps = []
            reused = recomputed = 0
            for ch, sig in enumerate(signals):
                fit = channels_out[ch]["fit"]
                if strategy == STRATEGY_WHOLE:
                    offset = channels_out[ch]["whole_delay"]
                    slope = 0.0
                else:
                    offset, slope = fit["offset"], fit["slope"]
                comp, hashes, ru, rc = compensate_channel(
                    sig, offset, slope, mode, seg_out_len, ch,
                    _input_hash(sig), self.cache,
                )
                reused += ru
                recomputed += rc
                comps.append(comp)
            compensated_by_mode[mode] = comps
            align = []
            ref_comp = comps[config.ref_channel]
            for ch, comp in enumerate(comps):
                if ch == config.ref_channel:
                    continue
                err = _marker_alignment_errors(
                    comp, ref_comp, fs, config.marker_period_s,
                    config.snr_threshold,
                )
                align.append({"channel": ch, **err})
            valid_errs = [a["max"] for a in align if a["max"] is not None]
            mode_stats[mode] = {
                "alignment": align,
                "max_error": max(valid_errs) if valid_errs else None,
                "within_tolerance": bool(
                    valid_errs
                    and max(valid_errs) <= config.align_tolerance
                ),
                "cache_reused": reused,
                "cache_recomputed": recomputed,
            }
            trace.event(
                "COMPENSATE",
                mode=mode,
                max_error=(
                    round(mode_stats[mode]["max_error"], 6)
                    if mode_stats[mode]["max_error"] is not None
                    else "na"
                ),
                cache_reused=reused,
                cache_recomputed=recomputed,
            )
            self._check_cancel()

        # 5. strategy comparison on the same material (no truncation)
        comparison = {"segmented": None, "whole": None}
        for name, use_whole in (("segmented", False), ("whole", True)):
            errs = []
            for ch, sig in enumerate(signals):
                if ch == config.ref_channel:
                    continue
                if use_whole:
                    offset, slope = channels_out[ch]["whole_delay"], 0.0
                else:
                    fit = channels_out[ch]["fit"]
                    offset, slope = fit["offset"], fit["slope"]
                comp = resample(sig, offset, slope, config.mode)
                err = _marker_alignment_errors(
                    comp, ref, fs, config.marker_period_s, config.snr_threshold
                )
                if err["rms"] is not None:
                    errs.append(err["rms"])
            comparison[name] = {
                "residual_rms_mean": float(np.mean(errs)) if errs else None
            }
        trace.event(
            "STRATEGY_COMPARE",
            segmented_rms=comparison["segmented"]["residual_rms_mean"],
            whole_rms=comparison["whole"]["residual_rms_mean"],
        )

        # 6. stitch selected mode, all-or-nothing
        os.makedirs(out_dir, exist_ok=True)
        matrix = stitch_channels(compensated_by_mode[config.mode])
        stitch_path = os.path.join(out_dir, "synced_multichannel.wav")
        write_multichannel_wav(stitch_path, matrix, fs)
        trace.event(
            "STITCH",
            path=os.path.basename(stitch_path),
            channels=matrix.shape[0],
            samples=matrix.shape[1],
            hash=content_hash(matrix)[:16],
        )

        # 7. report
        report_records = []
        for ch_out in channels_out:
            ch = ch_out["channel"]
            report_records.append(
                {
                    "channel": ch,
                    "fit": ch_out["fit"],
                    "segments": ch_out["segments"],
                    "diagnosis": ch_out["diagnosis"],
                    "jump_candidates": ch_out["jump_candidates"],
                    "markers_unavailable": ch_out["markers"]["unavailable"],
                    "input_hash": _input_hash(signals[ch]),
                }
            )
        report = build_report(
            dataset={
                "dir": os.path.basename(data_dir),
                "hash": ds_hash,
                "channels": n_ch,
                "fs": fs,
            },
            config=asdict(config),
            results=report_records,
        )
        report_path = os.path.join(out_dir, "report.json")
        save_report(report, report_path)
        trace.event("REPORT", path=os.path.basename(report_path),
                    chain_head=report["chain_head"][:16])
        trace.flush()

        return {
            "strategy": strategy,
            "strategy_reason": reason,
            "n_segments": n_segments,
            "channels": channels_out,
            "mode_stats": mode_stats,
            "mode_comparison": {
                m: mode_stats[m]["max_error"] for m in mode_stats
            },
            "strategy_comparison": comparison,
            "stitch_path": stitch_path,
            "stitch_shape": list(matrix.shape),
            "report_path": report_path,
            "trace_path": trace_path,
            "cache_stats": self.cache.stats(),
            "dataset_hash": ds_hash,
        }
