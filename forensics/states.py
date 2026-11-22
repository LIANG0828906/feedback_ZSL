"""Block-level state machine with four legal states.

States (as a raw diagram, backslashes intentionally escaped)::

    PENDING  --start-->  RUNNING  --commit-->  COMMITTED
    RUNNING  --fail-->   FAILED
    FAILED   --retry-->  RUNNING
    COMMITTED is terminal for a locked version.

Illegal transitions raise E_STATE and leave the block's state untouched.
"""

from .errors import ForensicsError, E_STATE

PENDING = "pending"
RUNNING = "running"
COMMITTED = "committed"
FAILED = "failed"

STATES = (PENDING, RUNNING, COMMITTED, FAILED)

LEGAL_TRANSITIONS = {
    PENDING: {RUNNING},
    RUNNING: {COMMITTED, FAILED},
    FAILED: {RUNNING},
    COMMITTED: set(),
}


class BlockState:
    def __init__(self, block_id):
        self.block_id = block_id
        self.state = PENDING
        self.attempts = 0
        self.last_error = None

    def transition(self, target):
        if target not in STATES:
            raise ForensicsError(
                E_STATE,
                "unknown target state %r for block %d" % (target, self.block_id),
                block_id=self.block_id,
            )
        allowed = LEGAL_TRANSITIONS[self.state]
        if target not in allowed:
            raise ForensicsError(
                E_STATE,
                "illegal transition %s -> %s for block %d"
                % (self.state, target, self.block_id),
                block_id=self.block_id,
            )
        self.state = target
        if target == RUNNING:
            self.attempts += 1
        return self.state

    def reset_to_pending(self):
        """Force-reset used when a cached snapshot is invalidated."""
        self.state = PENDING
        self.last_error = None

    def mark_failed(self, error):
        # Only legal from RUNNING; preserves state otherwise via E_STATE.
        self.transition(FAILED)
        self.last_error = error
