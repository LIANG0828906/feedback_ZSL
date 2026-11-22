"""Pipeline orchestration: parallel layout, serial in-order commits."""

from concurrent.futures import ThreadPoolExecutor
from threading import BoundedSemaphore, Event, RLock

from .checkpoint import CheckpointStore, new_run_dir
from .envelope import detect_segments
from .errors import PipelineError
from .hashing import content_hash
from .model import SubtitleResult
from .states import ProgressBoard
from .timing import extension_caps, resolve_subtitle
from .version import ALGORITHM_VERSION


class BufferBudget:
    """Per-worker text/sample buffer budget.

    Acquiring a slot blocks when the budget is exhausted, so subtitles queue
    instead of being silently dropped.
    """

    def __init__(self, max_buffers):
        self._sem = BoundedSemaphore(max_buffers)
        self.max_buffers = max_buffers

    def acquire(self):
        self._sem.acquire()

    def release(self):
        self._sem.release()


class Pipeline:
    def __init__(self, project, manifest, waveform=None, runs_root="runs",
                 max_workers=4, max_buffers=None, run_dir=None):
        self.project = project
        self.manifest = manifest
        self.waveform = waveform
        self.max_workers = max(1, max_workers)
        self.budget = BufferBudget(max_buffers or (2 * self.max_workers))
        self.board = ProgressBoard(len(project.subtitles))
        for sub in project.subtitles:
            self.board.register(sub.subtitle_id)
        self._pause = Event()
        self._stop = Event()
        self._commit_lock = RLock()
        self._sequence = 0
        self.project_hash = content_hash(manifest)
        self.params_hash = content_hash(project.params.to_dict())
        self.run_dir = run_dir or new_run_dir(runs_root, project.project_id)
        self.store = CheckpointStore(
            self.run_dir, self.project_hash, self.params_hash,
            ALGORITHM_VERSION)
        self.detected_segments = None
        self.failed = {}

    # ---- envelope -------------------------------------------------------
    def run_envelope_detection(self):
        if self.waveform is None:
            self.detected_segments = [
                (s.start_sample, s.end_sample) for s in self.project.segments]
            return self.detected_segments
        detected = detect_segments(
            self.waveform, self.project.frame_length,
            self.project.frame_shift, self.project.energy_threshold)
        expected = [(s.start_sample, s.end_sample)
                    for s in self.project.segments]
        if detected != expected:
            from .errors import E_DECODE
            raise PipelineError(
                E_DECODE,
                "detected envelope does not match declared segments",
                details={"detected": detected, "expected": expected})
        self.detected_segments = detected
        return detected

    # ---- worker ---------------------------------------------------------
    def _layout_one(self, subtitle, segment_map, bounds):
        self._wait_if_paused()
        if self._stop.is_set():
            return None
        self.budget.acquire()
        try:
            self.board.mark_layout(subtitle.subtitle_id)
            outcome = resolve_subtitle(
                subtitle, segment_map[subtitle.segment_ref],
                self.project.params, bounds, self.project.sample_rate)
            result = SubtitleResult(
                subtitle_id=subtitle.subtitle_id,
                segment_ref=subtitle.segment_ref,
                start_sample=outcome["start_sample"],
                end_sample=outcome["end_sample"],
                screens=outcome["screens"],
                required_duration_s=outcome["required_duration_s"],
                actual_duration_s=outcome["actual_duration_s"],
                tradeoffs=outcome["tradeoffs"],
                read_speed_cps=outcome["read_speed_cps"],
            )
            return result
        finally:
            self.budget.release()

    def _wait_if_paused(self):
        self._pause.wait()

    # ---- public control -------------------------------------------------
    def pause(self):
        self._pause.clear()

    def resume(self):
        self._pause.set()

    def stop(self):
        self._stop.set()
        self._pause.set()

    def run(self, parallel=True):
        """Layout every subtitle and commit in timeline (subtitle_id) order."""
        self.resume()
        self.run_envelope_detection()
        recovered, meta = self._recover()
        if meta == "invalidated":
            self.store.invalidate()
            self.store.write_meta()
        elif meta is None:
            self.store.write_meta()

        segment_map = self.project.segment_index()
        bounds = extension_caps(self.project.segments)
        pending = [s for s in self.project.subtitles
                   if not self.board.is_committed(s.subtitle_id)]

        if parallel and self.max_workers > 1:
            self._run_parallel(pending, segment_map, bounds)
        else:
            self._run_serial(pending, segment_map, bounds)
        return self.board.committed_results(), dict(self.failed)

    def _recover(self):
        results, meta = self.store.load()
        if isinstance(meta, str) and meta == "invalidated":
            return [], "invalidated"
        for result in results:
            if result.subtitle_id not in self.board._states:
                continue
            self.board.mark_layout(result.subtitle_id)
            self.board.commit_result(result.subtitle_id, result)
            self._sequence += 1
        return results, meta

    def _run_serial(self, pending, segment_map, bounds):
        for subtitle in pending:
            if self._stop.is_set():
                break
            try:
                result = self._layout_one(subtitle, segment_map, bounds)
            except PipelineError as exc:
                self._record_failure(subtitle, exc)
                continue
            if result is not None:
                self._commit_in_order(result)

    def _run_parallel(self, pending, segment_map, bounds):
        # Timeline order = subtitle_id order for the deterministic corpus.
        ordered = sorted(pending, key=lambda s: s.subtitle_id)
        next_index = 0
        results_by_id = {}
        with ThreadPoolExecutor(max_workers=self.max_workers,
                                thread_name_prefix="sub") as pool:
            futures = {
                pool.submit(self._layout_one, sub, segment_map, bounds):
                sub.subtitle_id for sub in ordered}
            for future in futures:
                sid = futures[future]
                try:
                    result = future.result()
                except PipelineError as exc:
                    self._record_failure(
                        next(sub for sub in ordered
                             if sub.subtitle_id == sid), exc)
                    continue
                if result is None:
                    continue
                results_by_id[sid] = result
                # Drain whatever is now available at the commit frontier.
                while next_index < len(ordered) and \
                        ordered[next_index].subtitle_id in results_by_id:
                    ready = results_by_id.pop(ordered[next_index].subtitle_id)
                    self._commit_in_order(ready)
                    next_index += 1

    def _commit_in_order(self, result):
        with self._commit_lock:
            self._sequence += 1
            self.store.append_commit(result, self._sequence)
            self.board.commit_result(result.subtitle_id, result)

    def _record_failure(self, subtitle, exc):
        with self._commit_lock:
            self.board.mark_failed(subtitle.subtitle_id, exc)
            self.failed[subtitle.subtitle_id] = exc

    def retry_failed(self, parallel=True):
        """Retry only failed subtitles; committed ones stay locked."""
        failed_ids = list(self.failed.keys())
        for sid in failed_ids:
            self.board.reset_for_retry(sid)
        self.failed.clear()
        segment_map = self.project.segment_index()
        bounds = extension_caps(self.project.segments)
        pending = [s for s in self.project.subtitles
                   if s.subtitle_id in failed_ids]
        if parallel and self.max_workers > 1:
            self._run_parallel(pending, segment_map, bounds)
        else:
            self._run_serial(pending, segment_map, bounds)
        return self.board.committed_results(), dict(self.failed)
