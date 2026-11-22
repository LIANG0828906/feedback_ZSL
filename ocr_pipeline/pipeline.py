"""Pipeline orchestration: pausable, resumable, optionally parallel.

Recognition work may run in a thread pool, but checkpoint writes are
serialized through the CheckpointStore lock and commits happen in canonical
(image id, interval index) order, so parallel and serial runs produce
identical per-interval results and identical commit order.
"""

from __future__ import annotations

import hashlib
import json
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional, Tuple

from . import engine, mapper
from .checkpoint import CheckpointStore
from .generator import gt_regions_of
from .model import ImageResult, TextLine
from .schemas.exporter import export_bytes
from .state import IntervalState, build_intervals, progress


@dataclass
class PipelineConfig:
    data_dir: Path
    source_version: str = "3.0"
    merge_threshold: float = engine.DEFAULT_MERGE_THRESHOLD
    conf_threshold: float = engine.DEFAULT_CONF_THRESHOLD
    lines_per_interval: int = 2
    max_workers: int = 1
    buffer_limit: int = 1  # max image buffers held per worker thread


def line_to_dict(line: TextLine) -> Dict:
    return {
        "line_id": line.line_id,
        "quad": [[x, y] for x, y in line.quad],
        "text": line.text,
        "char_conf": list(line.char_conf) if line.char_conf is not None else None,
        "lang": line.lang,
        "order_index": line.order_index,
    }


def line_from_dict(doc: Dict) -> TextLine:
    conf = doc["char_conf"]
    return TextLine(
        line_id=doc["line_id"],
        quad=tuple(tuple(pt) for pt in doc["quad"]),
        text=doc["text"],
        char_conf=tuple(conf) if conf is not None else None,
        lang=doc["lang"],
        order_index=doc["order_index"],
    )


class Pipeline:
    def __init__(self, config: PipelineConfig) -> None:
        self.config = config
        self.data_dir = Path(config.data_dir)
        self.store = CheckpointStore(self.data_dir / "checkpoints")
        self.param_version = engine.param_version(
            config.merge_threshold, config.conf_threshold
        )
        self.results: Dict[str, ImageResult] = {}
        self._buffer_sem = threading.BoundedSemaphore(
            max(1, config.max_workers) * max(1, config.buffer_limit)
        )

    # -- dataset access --------------------------------------------------------

    def _dataset_manifest(self) -> Dict:
        return json.loads(
            (self.data_dir / "manifest.json").read_text(encoding="utf-8")
        )

    def _load_gt(self, image_id: str) -> Dict:
        path = self.data_dir / "ground_truth" / f"{image_id}.json"
        return json.loads(path.read_text(encoding="utf-8"))

    def _content_hash(self, gt: Dict) -> str:
        payload = (self.data_dir / gt["image_file"]).read_bytes()
        return hashlib.sha256(payload).hexdigest()

    # -- recognition -------------------------------------------------------------

    def _recognize_interval(self, image_id: str, gt: Dict, start: int, end: int):
        """Worker body: holds at most buffer_limit image buffers per thread."""
        with self._buffer_sem:
            lines = engine.detect_and_recognize(
                image_id,
                gt_regions_of(gt),
                merge_threshold=self.config.merge_threshold,
                conf_threshold=self.config.conf_threshold,
            )
        return tuple(lines[start:end])

    # -- main run ------------------------------------------------------------------

    def run(self, cancel_event: Optional[threading.Event] = None) -> Dict:
        cfg = self.config
        summary = {"images": {}, "completed": True}
        tasks: List[Tuple[str, int, int, int]] = []  # image_id, interval, start, end
        all_pairs: List[Tuple[str, int]] = []
        image_ctx: Dict[str, Dict] = {}

        for entry in sorted(
            self._dataset_manifest()["images"], key=lambda e: e["image_id"]
        ):
            image_id = entry["image_id"]
            gt = self._load_gt(image_id)
            content_hash = self._content_hash(gt)
            lines = engine.detect_and_recognize(
                image_id,
                gt_regions_of(gt),
                merge_threshold=cfg.merge_threshold,
                conf_threshold=cfg.conf_threshold,
            )
            mapper.validate_lines(image_id, lines)
            intervals = build_intervals(image_id, len(lines), cfg.lines_per_interval)

            committed = 0
            snapshot = self.store.load(image_id)
            if snapshot is not None:
                manifest, _records = snapshot
                if self.store.snapshot_matches(
                    manifest,
                    content_hash=content_hash,
                    source_version=cfg.source_version,
                    param_version=self.param_version,
                    algo_version=engine.ALGO_VERSION,
                ):
                    committed = manifest["committed_intervals"]
                else:
                    self.store.invalidate(image_id)

            self.store.write_manifest(
                image_id,
                {
                    "image_id": image_id,
                    "content_hash": content_hash,
                    "source_version": cfg.source_version,
                    "param_version": self.param_version,
                    "algo_version": engine.ALGO_VERSION,
                    "committed_intervals": committed,
                    "total_intervals": len(intervals),
                },
            )
            image_ctx[image_id] = {
                "gt": gt,
                "content_hash": content_hash,
                "intervals": intervals,
                "lines": lines,
                "committed": committed,
            }
            # Restore persisted commit state onto the fresh interval objects
            # (direct restore, not a state-machine transition).
            for iv in intervals[:committed]:
                iv.state = IntervalState.COMMITTED
                iv.frozen_params = (cfg.merge_threshold, cfg.conf_threshold)
                iv.frozen_source_version = cfg.source_version
            for iv in intervals:
                all_pairs.append((image_id, iv.interval_index))
            for iv in intervals[committed:]:
                tasks.append((image_id, iv.interval_index, iv.line_start, iv.line_end))

        # Canonical commit sequence: stable across cancel/resume and identical
        # for serial and parallel runs.
        seq_map = {pair: seq for seq, pair in enumerate(all_pairs)}

        # Recognition may be parallel; results are committed in canonical order.
        computed: Dict[Tuple[str, int], Tuple[TextLine, ...]] = {}
        if cfg.max_workers > 1:
            with ThreadPoolExecutor(max_workers=cfg.max_workers) as pool:
                futures = {
                    (image_id, idx): pool.submit(
                        self._recognize_interval,
                        image_id,
                        image_ctx[image_id]["gt"],
                        start,
                        end,
                    )
                    for image_id, idx, start, end in tasks
                }
                for key, future in futures.items():
                    computed[key] = future.result()
        else:
            for image_id, idx, start, end in tasks:
                computed[(image_id, idx)] = self._recognize_interval(
                    image_id, image_ctx[image_id]["gt"], start, end
                )

        for image_id, idx, _start, _end in tasks:
            if cancel_event is not None and cancel_event.is_set():
                summary["completed"] = False
                break
            ctx = image_ctx[image_id]
            iv = ctx["intervals"][idx]
            iv.transition(IntervalState.RECOGNIZING)
            interval_lines = computed[(image_id, idx)]
            iv.commit(
                cfg.merge_threshold, cfg.conf_threshold, cfg.source_version
            )
            record = {
                "image_id": image_id,
                "interval_index": idx,
                "line_start": iv.line_start,
                "line_end": iv.line_end,
                "lines": [line_to_dict(ln) for ln in interval_lines],
                "content_hash": ctx["content_hash"],
                "source_version": cfg.source_version,
                "param_version": self.param_version,
                "algo_version": engine.ALGO_VERSION,
                "params": {
                    "merge_threshold": cfg.merge_threshold,
                    "conf_threshold": cfg.conf_threshold,
                },
                "commit_seq": seq_map[(image_id, idx)],
            }
            self.store.commit_interval(image_id, idx, record)
            ctx["committed"] += 1
            self.store.write_manifest(
                image_id,
                {
                    "image_id": image_id,
                    "content_hash": ctx["content_hash"],
                    "source_version": cfg.source_version,
                    "param_version": self.param_version,
                    "algo_version": engine.ALGO_VERSION,
                    "committed_intervals": ctx["committed"],
                    "total_intervals": len(ctx["intervals"]),
                },
            )
        for image_id, ctx in image_ctx.items():
            done, total = progress(ctx["intervals"])
            summary["images"][image_id] = {
                "committed_intervals": done,
                "total_intervals": total,
            }
            self.results[image_id] = ImageResult(
                image_id=image_id,
                width=ctx["gt"]["width"],
                height=ctx["gt"]["height"],
                lines=tuple(ctx["lines"]),
                source_version=cfg.source_version,
            )
        return summary

    # -- export --------------------------------------------------------------------

    def export(self, target_version: str) -> Dict[str, List[Dict]]:
        """Export all image results; returns per-image E_LOSSY reports."""
        out_dir = self.data_dir / "exports" / target_version
        out_dir.mkdir(parents=True, exist_ok=True)
        reports: Dict[str, List[Dict]] = {}
        for image_id in sorted(self.results):
            payload, warnings = export_bytes(self.results[image_id], target_version)
            (out_dir / f"{image_id}.json").write_bytes(payload)
            reports[image_id] = [w.__dict__ for w in warnings]
        report_path = self.data_dir / "exports" / f"lossy_report_{target_version}.json"
        report_path.write_text(
            json.dumps(reports, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        return reports
