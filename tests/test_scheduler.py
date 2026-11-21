import pytest

from loudmeter.config import ConfigError, MeterConfig
from loudmeter.errors import BusyError
from loudmeter.scheduler import Scheduler, TraceLog


def _fns(asset, n):
    return [(i, (lambda i=i: f"{asset}-{i}")) for i in range(n)]


def test_busy_on_duplicate_asset():
    s = Scheduler(MeterConfig(), TraceLog())
    s.submit_asset("a", _fns("a", 2))
    with pytest.raises(BusyError):
        s.submit_asset("a", _fns("a", 2))


def test_parallelism_and_budget_not_raisable():
    cfg = MeterConfig(parallelism=2, memory_budget_bytes=10_000_000)
    with pytest.raises(ConfigError):
        cfg.update(parallelism=4)
    with pytest.raises(ConfigError):
        cfg.update(memory_budget_bytes=20_000_000)
    cfg2 = cfg.update(parallelism=1)  # lowering is allowed
    assert cfg2.parallelism == 1


def test_budget_forces_waiting():
    mem = MeterConfig().task_memory_bytes()
    cfg = MeterConfig(parallelism=4, memory_budget_bytes=mem * 2)
    trace = TraceLog()
    s = Scheduler(cfg, trace)
    s.submit_asset("a", _fns("a", 3))
    s.submit_asset("b", _fns("b", 3))
    s.run()
    waits = [e for e in trace.entries if e["event"] == "wait_budget"]
    assert waits, "expected tasks to wait once the budget is exhausted"
    used = [e["budget_used"] for e in trace.entries if "budget_used" in e]
    assert all(u <= cfg.memory_budget_bytes for u in used)
    assert len(s.results) == 6


def test_same_asset_blocks_submitted_in_order():
    trace = TraceLog()
    s = Scheduler(MeterConfig(), trace)
    s.submit_asset("a", _fns("a", 4))
    s.submit_asset("b", _fns("b", 2))
    submits = [e for e in trace.entries if e["event"] == "submit"
               and e["asset_id"] == "a"]
    assert [e["block_index"] for e in submits] == [0, 1, 2, 3]
    seqs = [e["seq"] for e in trace.entries if e["event"] == "submit"]
    assert seqs == sorted(seqs)


def test_deterministic_trace():
    traces = []
    for _ in range(2):
        trace = TraceLog()
        s = Scheduler(MeterConfig(parallelism=3), trace)
        s.submit_asset("a", _fns("a", 5))
        s.submit_asset("b", _fns("b", 5))
        s.run()
        traces.append(trace.entries)
    assert traces[0] == traces[1]
