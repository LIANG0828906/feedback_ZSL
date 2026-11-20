import unittest

from replay.engine import DeterministicEngine, OperationBuffer
from replay.errors import ErrorCode
from replay.models import OP_MOVE, OP_SPEND, OP_SUPPLY, Operation, RoomState


def make_state() -> RoomState:
    return RoomState(
        tick=0,
        entities={"e0": {"pos": [0.0, 0.0], "vel": [0.0, 0.0], "alive": True, "owner": 0}},
        resources={"ammo": 10},
    )


def op(player, client_seq, arrival, tick, op_type=OP_MOVE, payload=None):
    return Operation(player, client_seq, arrival, tick, op_type, payload or {"entity_id": "e0", "dx": 1.0, "dy": 0.0})


class OperationBufferTest(unittest.TestCase):
    def test_duplicate_client_seq_kept_once(self):
        buffer = OperationBuffer()
        self.assertTrue(buffer.add(op(0, 1, 0, 1)))
        self.assertFalse(buffer.add(op(0, 1, 5, 1)))
        due, rejected = buffer.drain_due(1, late_window=2)
        self.assertEqual(len(due), 1)
        self.assertEqual(len(buffer.duplicates), 1)

    def test_late_op_rejected_with_code(self):
        buffer = OperationBuffer()
        buffer.add(op(0, 1, 0, tick=1))
        # 窗口下界 = 5 - 2 = 3，tick=1 的操作迟到。
        due, rejected = buffer.drain_due(current_tick=5, late_window=2)
        self.assertEqual(due, [])
        self.assertEqual(len(rejected), 1)
        self.assertEqual(rejected[0].code, ErrorCode.E_OP_LATE.value)

    def test_stable_order_independent_of_insertion(self):
        ops_a = [op(1, 0, 2, 1), op(0, 0, 1, 1), op(0, 1, 0, 1)]
        ops_b = list(reversed(ops_a))
        ordered = []
        for ops in (ops_a, ops_b):
            engine = DeterministicEngine(make_state(), late_window=2)
            engine.ingest(ops)
            report = engine.step()
            ordered.append([(o.player_id, o.client_seq, o.arrival_seq) for o in report.applied])
        self.assertEqual(ordered[0], ordered[1])
        self.assertEqual(ordered[0], [(0, 1, 0), (0, 0, 1), (1, 0, 2)])


class DeterminismTest(unittest.TestCase):
    def test_same_inputs_byte_identical_chain(self):
        ops = [
            op(0, 0, 0, 1),
            op(0, 1, 1, 2, OP_SPEND, {"resource": "ammo", "amount": 3}),
            op(0, 2, 2, 3, OP_SUPPLY, {"resource": "ammo", "amount": 2}),
        ]
        chains = []
        for _ in range(3):
            engine = DeterministicEngine(make_state(), late_window=2)
            engine.ingest(ops)
            hashes = [engine.step().state_hash for _ in range(3)]
            chains.append(hashes)
        self.assertEqual(chains[0], chains[1])
        self.assertEqual(chains[1], chains[2])

    def test_resource_count_deterministic(self):
        engine = DeterministicEngine(make_state(), late_window=2)
        engine.ingest([op(0, 0, 0, 1, OP_SPEND, {"resource": "ammo", "amount": 4})])
        engine.step()
        self.assertEqual(engine.state.resources["ammo"], 6)

    def test_hash_chain_links_to_previous(self):
        engine = DeterministicEngine(make_state(), late_window=2)
        engine.ingest([op(0, 0, 0, 1), op(0, 1, 1, 2)])
        first = engine.step()
        second = engine.step()
        self.assertEqual(first.prev_hash, DeterministicEngine.GENESIS_HASH)
        self.assertEqual(second.prev_hash, first.state_hash)


if __name__ == "__main__":
    unittest.main()
