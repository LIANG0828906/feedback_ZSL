"""结算语义：确定性、去重、迟到、配额、稳定顺序。"""

from replay.canon import GENESIS_CHAIN, canonical_bytes, chain_hash
from replay.engine import ReplayEngine
from replay.generator import generate_sample

from conftest import cmd, make_match


def test_determinism_same_sample_same_chain():
    sample = generate_sample(seed=23, players=4, frames=40, disorder=3)
    e1 = ReplayEngine(sample)
    e1.run()
    e2 = ReplayEngine(sample)
    e2.run()
    assert e1.chain_tip == e2.chain_tip
    assert canonical_bytes(e1.trace_lines()) == \
        canonical_bytes(e2.trace_lines())


def test_duplicate_client_seq_first_wins():
    match = make_match([
        cmd("p1", 0, recv=0, dx=1),
        cmd("p1", 0, recv=1, dx=-1),
    ], frames=2)
    engine = ReplayEngine(match)
    engine.run()
    events = [e for r in engine.records for e in r["events"]]
    assert sum(1 for e in events if e["type"] == "CMD_APPLIED") == 1
    assert any(e["type"] == "E_CMD_DUP" for e in events)
    idx = engine.state.unit_ids.index("p1-u0")
    assert int(engine.state.vel[idx, 0]) == 1


def test_late_command_rejected():
    match = make_match([
        cmd("p1", 0, submit=0, recv=0, recv_tick=5),
    ], frames=6, window=2)
    engine = ReplayEngine(match)
    engine.run()
    events = [e for r in engine.records for e in r["events"]]
    late = [e for e in events if e["type"] == "E_CMD_LATE"]
    assert len(late) == 1
    assert late[0]["player"] == "p1"
    assert not any(e["type"] == "CMD_APPLIED" for e in events)
    idx = engine.state.unit_ids.index("p1-u0")
    assert int(engine.state.vel[idx, 0]) == 0


def test_quota_exceeded_rejected_and_isolated():
    match = make_match([
        cmd("p1", 0, recv=0, cost=2),
        cmd("p1", 1, recv=1, cost=2),
    ], frames=2, quota=3)
    engine = ReplayEngine(match)
    engine.run()
    frame0 = engine.records[0]
    exceeded = [e for e in frame0["events"]
                if e["type"] == "E_QUOTA_EXCEEDED"]
    assert len(exceeded) == 1
    assert exceeded[0]["frame"] == 0
    assert exceeded[0]["player"] == "p1"
    assert frame0["quota_used"]["p1"] == 2
    assert frame0["applied"] == ["p1:0"]
    assert engine.quota["p1"][0] == 3


def test_stable_order_by_recv_seq():
    m1 = make_match([
        cmd("p1", 0, recv=1, op="move", dx=1),
        cmd("p1", 1, recv=0, op="stop"),
    ], frames=2)
    e1 = ReplayEngine(m1)
    e1.run()
    idx = e1.state.unit_ids.index("p1-u0")
    assert int(e1.state.vel[idx, 0]) == 1
    m2 = make_match([
        cmd("p1", 0, recv=0, op="move", dx=1),
        cmd("p1", 1, recv=1, op="stop"),
    ], frames=2)
    e2 = ReplayEngine(m2)
    e2.run()
    assert int(e2.state.vel[idx, 0]) == 0
    assert e1.chain_tip != e2.chain_tip


def test_hash_chain_links():
    sample = generate_sample(seed=11, players=3, frames=16, disorder=1)
    engine = ReplayEngine(sample)
    engine.run()
    prev = GENESIS_CHAIN
    for entry in engine.hash_chain:
        assert chain_hash(prev, entry["state_hash"]) == entry["chain_hash"]
        prev = entry["chain_hash"]
