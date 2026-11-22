from __future__ import annotations

import unittest

from vector_workbench.errors import ErrorCode, WorkbenchError
from vector_workbench.model import CellStatus, Stage
from vector_workbench.state import PathStateMachine


class _DummyPath:
    def __init__(self, path_id: str) -> None:
        self.id = path_id


class StateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.machine = PathStateMachine([_DummyPath("p0"), _DummyPath("p1")])

    def test_illegal_transition_preserves_state(self) -> None:
        with self.assertRaises(WorkbenchError) as raised:
            self.machine.commit("p0", Stage.FLATTEN)
        self.assertEqual(raised.exception.code, ErrorCode.E_STATE)
        self.assertEqual(
            self.machine.status("p0", Stage.FLATTEN), CellStatus.PENDING
        )
        self.machine.begin("p0", Stage.FLATTEN)
        with self.assertRaises(WorkbenchError):
            self.machine.begin("p0", Stage.FLATTEN)
        self.assertEqual(
            self.machine.status("p0", Stage.FLATTEN), CellStatus.PROCESSING
        )
        self.machine.commit("p0", Stage.FLATTEN)
        self.assertEqual(self.machine.committed_cells, 1)

    def test_prerequisite_and_previous_compose_gate(self) -> None:
        with self.assertRaises(WorkbenchError) as raised:
            self.machine.begin("p0", Stage.COVERAGE)
        self.assertEqual(raised.exception.code, ErrorCode.E_STATE)
        self.machine.begin("p0", Stage.FLATTEN)
        self.machine.commit("p0", Stage.FLATTEN)
        self.machine.begin("p0", Stage.COVERAGE)
        self.machine.commit("p0", Stage.COVERAGE)
        self.machine.begin("p1", Stage.FLATTEN)
        self.machine.commit("p1", Stage.FLATTEN)
        self.machine.begin("p1", Stage.COVERAGE)
        self.machine.commit("p1", Stage.COVERAGE)
        with self.assertRaises(WorkbenchError) as raised:
            self.machine.begin("p1", Stage.COMPOSE)
        self.assertEqual(raised.exception.code, ErrorCode.E_STATE)
        self.assertEqual(
            self.machine.status("p1", Stage.COMPOSE), CellStatus.PENDING
        )
        self.machine.begin("p0", Stage.COMPOSE)
        self.machine.commit("p0", Stage.COMPOSE)
        self.machine.begin("p1", Stage.COMPOSE)
        self.machine.commit("p1", Stage.COMPOSE)
        self.assertTrue(self.machine.all_committed())

    def test_failed_stage_can_retry(self) -> None:
        self.machine.begin("p0", Stage.FLATTEN)
        error = WorkbenchError(
            ErrorCode.E_STAGE, "injected", path_id="p0", stage="flatten"
        )
        self.machine.fail("p0", Stage.FLATTEN, error)
        self.assertEqual(
            self.machine.status("p0", Stage.FLATTEN), CellStatus.FAILED
        )
        self.assertIn(Stage.FLATTEN, self.machine.errors("p0"))
        self.machine.begin("p0", Stage.FLATTEN)
        self.assertEqual(
            self.machine.status("p0", Stage.FLATTEN), CellStatus.PROCESSING
        )
        self.assertNotIn(Stage.FLATTEN, self.machine.errors("p0"))


if __name__ == "__main__":
    unittest.main()

