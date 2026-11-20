"""增强引擎：增量重算、原子发布、快照恢复与原子导出。"""

from __future__ import annotations

import hashlib
import json
import os
import threading
from pathlib import Path

import numpy as np

from . import wavio
from .cache import PIPELINE_VERSION, ResultCache, channel_cache_key
from .config import EnhanceConfig
from .control import CancelToken
from .errors import CancelledError, RecoveryInvalidError
from .noise import estimate_noise
from .pipeline import process_channel
from .snapshot import SnapshotStore
from .trace import TraceLogger


def _json_dump_atomic(path: Path, payload: dict) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(payload, fh, sort_keys=True, ensure_ascii=False, indent=1)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


class PublishedStore:
    """发布目录：版本子目录 + current 符号链接原子切换。"""

    def __init__(self, pub_dir):
        self.dir = Path(pub_dir)
        self.dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def _current_link(self) -> Path:
        return self.dir / "current"

    def publish(self, enhanced: np.ndarray, report: dict) -> Path:
        with self._lock:
            existing = [
                int(p.name[2:]) for p in self.dir.glob("v_*") if p.name[2:].isdigit()
            ]
            version = max(existing) + 1 if existing else 0
            vdir = self.dir / f"v_{version:06d}"
            vdir.mkdir()
            wavio.write_wav(vdir / "enhanced.wav", enhanced)
            _json_dump_atomic(vdir / "report.json", report)
            tmp_link = self.dir / "current.tmp"
            if tmp_link.exists() or tmp_link.is_symlink():
                tmp_link.unlink()
            tmp_link.symlink_to(vdir.name)
            os.replace(tmp_link, self._current_link())
            return vdir

    def get(self):
        """查询接口：替换期间始终返回完整旧版或完整新版。"""
        link = self._current_link()
        if not link.exists():
            return None
        vdir = link.resolve()
        wav_path = vdir / "enhanced.wav"
        rep_path = vdir / "report.json"
        if not wav_path.exists() or not rep_path.exists():
            return None
        data, rate = wavio.read_wav(wav_path)
        with open(rep_path, "r", encoding="utf-8") as fh:
            report = json.load(fh)
        return {"version_dir": str(vdir), "data": data, "rate": rate, "report": report}


class Engine:
    def __init__(self, workdir):
        self.workdir = Path(workdir)
        self.workdir.mkdir(parents=True, exist_ok=True)
        self.cache = ResultCache(self.workdir / "cache")
        self.snapshots = SnapshotStore(self.workdir / "snapshots")
        self.published = PublishedStore(self.workdir / "published")
        self.traces_dir = self.workdir / "traces"
        self.traces_dir.mkdir(parents=True, exist_ok=True)

    def run(
        self,
        wav_path,
        config: EnhanceConfig,
        cancel_token: CancelToken | None = None,
        trace_name: str = "trace.jsonl",
        resume: bool = False,
        crash_after_channel: int | None = None,
    ) -> dict:
        """执行增强并原子发布；返回报告字典。"""
        token = cancel_token or CancelToken()
        check = token.check
        data, rate = wavio.read_wav(wav_path)
        n_samples, n_channels = data.shape
        input_hash = wavio.file_hash(wav_path)
        config_hash = config.content_hash()
        ch_hashes = [
            hashlib.sha256(np.ascontiguousarray(data[:, c]).tobytes()).hexdigest()
            for c in range(n_channels)
        ]
        keys = [
            channel_cache_key(ch_hashes[c], config.channel_hash(c))
            for c in range(n_channels)
        ]

        completed: dict = {}
        if resume:
            snap = self.snapshots.load_valid_chain()
            if snap["input_hash"] != input_hash or snap["config_hash"] != config_hash:
                raise RecoveryInvalidError(
                    "快照与当前输入或参数不匹配",
                    snapshot_input=snap["input_hash"],
                    input_hash=input_hash,
                )
            completed = dict(snap["completed"])

        trace = TraceLogger(self.traces_dir / trace_name)
        try:
            trace.log(
                "run_start",
                input_hash=input_hash,
                config_hash=config_hash,
                channels=n_channels,
                samples=n_samples,
                resume=resume,
            )
            estimates, interval, onsets = estimate_noise(
                data, config.noise, config.metric.signal_window
            )
            p_start, p_end = interval
            trace.log(
                "noise_estimated",
                interval=[p_start, p_end],
                channels=[
                    {"channel": e.channel, "rms": e.rms, "snr_db": e.snr_db}
                    for e in estimates
                ],
            )

            enhanced = np.zeros_like(data)
            channel_reports = []
            for ch in range(n_channels):
                check()
                key = keys[ch]
                if str(ch) in completed and completed[str(ch)] == key:
                    hit = self.cache.get(key)
                    if hit is not None:
                        enhanced[:, ch], meta = hit
                        channel_reports.append(meta["report"])
                        trace.log(
                            "channel",
                            channel=ch,
                            action="reuse",
                            source="snapshot",
                            cache_key=key,
                            bands=[b[0] for b in config.bands],
                        )
                        continue
                hit = self.cache.get(key)
                if hit is not None:
                    enhanced[:, ch], meta = hit
                    channel_reports.append(meta["report"])
                    trace.log(
                        "channel",
                        channel=ch,
                        action="reuse",
                        source="cache",
                        cache_key=key,
                        bands=[b[0] for b in config.bands],
                    )
                else:
                    est = estimates[ch]
                    result = process_channel(
                        data[:, ch],
                        data[p_start:p_end, ch],
                        int(onsets[ch]),
                        est.rms,
                        est.snr_db,
                        interval,
                        config,
                        ch,
                        check,
                    )
                    enhanced[:, ch] = result.enhanced
                    report_ch = {
                        "channel": ch,
                        "onset": result.onset,
                        "noise_rms_in": result.noise_rms_in,
                        "snr_in_db": result.snr_in_db,
                        "residual_ratio_mean": result.residual_ratio_mean,
                        "residual_ratio_max": result.residual_ratio_max,
                        "bands": result.band_metrics,
                    }
                    self.cache.put(key, result.enhanced, {"report": report_ch})
                    channel_reports.append(report_ch)
                    trace.log(
                        "channel",
                        channel=ch,
                        action="recompute",
                        source="compute",
                        cache_key=key,
                        bands=[b[0] for b in config.bands],
                    )
                completed[str(ch)] = key
                self.snapshots.write(ch, input_hash, config_hash, completed)
                trace.log("snapshot", channel=ch, seq=ch)
                if crash_after_channel is not None and ch == crash_after_channel:
                    trace.log("crash_simulated", channel=ch)
                    trace.close()
                    os._exit(1)

            check()
            report = self._build_report(
                config, input_hash, config_hash, interval, estimates,
                channel_reports, enhanced,
            )
            vdir = self.published.publish(enhanced, report)
            trace.log(
                "published",
                version=vdir.name,
                output_hash=wavio.data_hash(enhanced),
            )
            trace.log("run_complete")
            return report
        except CancelledError:
            trace.log("cancelled")
            raise
        finally:
            trace.close()

    def _build_report(
        self, config, input_hash, config_hash, interval, estimates,
        channel_reports, enhanced,
    ) -> dict:
        degraded = []
        for rep in channel_reports:
            for band in rep["bands"]:
                if band["degraded"]:
                    degraded.append(
                        {
                            "channel": rep["channel"],
                            "band": band["band"],
                            "reason": band["reason"],
                        }
                    )
        return {
            "format": "ir_enhancer.report/1",
            "pipeline_version": PIPELINE_VERSION,
            "param_version": config_hash,
            "input_hash": input_hash,
            "output_hash": wavio.data_hash(enhanced),
            "sample_rate": config.sample_rate,
            "pretrigger_interval": list(interval),
            "noise_floor": [
                {
                    "channel": e.channel,
                    "rms": e.rms,
                    "power": e.power,
                    "snr_in_db": e.snr_db,
                }
                for e in estimates
            ],
            "channels": channel_reports,
            "degraded_bands": degraded,
        }

    def export(self, out_dir) -> dict:
        """将当前发布结果以临时文件 + 原子重命名导出 WAV 与 JSON 报告。"""
        current = self.published.get()
        if current is None:
            raise RecoveryInvalidError("不存在已发布结果，无法导出")
        out = Path(out_dir)
        out.mkdir(parents=True, exist_ok=True)
        wavio.write_wav(out / "enhanced.wav.tmp", current["data"])
        os.replace(out / "enhanced.wav.tmp", out / "enhanced.wav")
        _json_dump_atomic(out / "report.json", current["report"])
        return {
            "wav": str(out / "enhanced.wav"),
            "report": str(out / "report.json"),
        }
