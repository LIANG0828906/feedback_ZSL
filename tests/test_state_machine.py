"""节目段状态机合法/非法迁移测试（E_STATE 且原状态保持）。"""
from __future__ import annotations

import unittest

from loudbench.core.errors import StateError
from loudbench.core.states import SegmentState, SegmentStateMachine


class StateMachineTest(unittest.TestCase):
    def test_happy_path(self) -> None:
        sm = SegmentStateMachine(0)
        self.assertIs(sm.state, SegmentState.PENDING)
        sm.transition(SegmentState.PROCESSING)
        sm.transition(SegmentState.COMMITTED)
        self.assertIs(sm.state, SegmentState.COMMITTED)

    def test_failed_retry_path(self) -> None:
        sm = SegmentStateMachine(1)
        sm.transition(SegmentState.PROCESSING)
        sm.transition(SegmentState.FAILED, reason="boom")
        self.assertEqual(sm.fail_reason, "boom")
        sm.transition(SegmentState.PROCESSING)
        sm.transition(SegmentState.COMMITTED)
        self.assertIs(sm.state, SegmentState.COMMITTED)

    def test_illegal_transition_keeps_state(self) -> None:
        sm = SegmentStateMachine(2)
        with self.assertRaises(StateError) as ctx:
            sm.transition(SegmentState.COMMITTED)  # 跳过 PROCESSING
        self.assertEqual(ctx.exception.code, "E_STATE")
        self.assertIs(sm.state, SegmentState.PENDING)

    def test_committed_is_terminal(self) -> None:
        sm = SegmentStateMachine(3)
        sm.transition(SegmentState.PROCESSING)
        sm.transition(SegmentState.COMMITTED)
        for target in (SegmentState.PENDING, SegmentState.PROCESSING,
                       SegmentState.FAILED):
            with self.assertRaises(StateError):
                sm.transition(target)
            self.assertIs(sm.state, SegmentState.COMMITTED)

    def test_dependency_gate_blocks_processing(self) -> None:
        # 已提交段不可再进入处理中（依赖/终态保护）
        sm = SegmentStateMachine(4)
        sm.transition(SegmentState.PROCESSING)
        sm.transition(SegmentState.COMMITTED)
        self.assertFalse(sm.can_transition(SegmentState.PROCESSING))


if __name__ == "__main__":
    unittest.main()
