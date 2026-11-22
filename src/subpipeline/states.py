"""Per-subtitle state machine and commit boundary."""

from threading import RLock

from .errors import E_STATE, PipelineError
from .model import (
    LEGAL_TRANSITIONS, STATE_COMMITTED, STATE_FAILED, STATE_LAYOUT,
    STATE_PENDING, SubtitleResult,
)


class SubtitleState:
    def __init__(self, subtitle_id):
        self.subtitle_id = subtitle_id
        self.state = STATE_PENDING
        self.result = None
        self.error = None

    def transition(self, target):
        allowed = LEGAL_TRANSITIONS[self.state]
        if target not in allowed:
            raise PipelineError(
                E_STATE,
                f"illegal transition {self.state} -> {target}",
                subtitle_id=self.subtitle_id,
                details={"from": self.state, "to": target})
        self.state = target

    def commit(self, result):
        if not isinstance(result, SubtitleResult):
            raise TypeError("commit expects a SubtitleResult")
        self.transition(STATE_COMMITTED)
        self.result = result
        self.error = None

    def fail(self, error):
        self.transition(STATE_FAILED)
        self.error = error

    def retry(self):
        self.transition(STATE_PENDING)
        self.error = None


class ProgressBoard:
    """Thread-safe registry of subtitle states and committed results."""

    def __init__(self, total):
        self._lock = RLock()
        self._states = {}
        self.total = total
        self._committed = {}

    def register(self, subtitle_id):
        with self._lock:
            self._states[subtitle_id] = SubtitleState(subtitle_id)

    def get(self, subtitle_id):
        with self._lock:
            return self._states[subtitle_id]

    def snapshot(self):
        with self._lock:
            return {sid: st.state for sid, st in self._states.items()}

    @property
    def committed_count(self):
        with self._lock:
            return len(self._committed)

    @property
    def progress(self):
        with self._lock:
            return len(self._committed), self.total

    def commit_result(self, subtitle_id, result):
        with self._lock:
            st = self._states[subtitle_id]
            st.commit(result)
            self._committed[subtitle_id] = result

    def mark_failed(self, subtitle_id, error):
        with self._lock:
            st = self._states[subtitle_id]
            st.fail(error)

    def mark_layout(self, subtitle_id):
        with self._lock:
            self._states[subtitle_id].transition(STATE_LAYOUT)

    def reset_for_retry(self, subtitle_id):
        with self._lock:
            self._states[subtitle_id].retry()

    def committed_results(self):
        with self._lock:
            return [self._committed[sid] for sid in sorted(self._committed)]

    def is_committed(self, subtitle_id):
        with self._lock:
            return self._states[subtitle_id].state == STATE_COMMITTED
