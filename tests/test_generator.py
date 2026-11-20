"""样本生成器确定性。"""

from replay.canon import canonical_bytes
from replay.engine import ReplayEngine
from replay.generator import generate_sample


def test_same_seed_byte_identical():
    s1 = generate_sample(seed=99, players=3, frames=20, disorder=3)
    s2 = generate_sample(seed=99, players=3, frames=20, disorder=3)
    assert canonical_bytes(s1) == canonical_bytes(s2)
    e1 = ReplayEngine(s1)
    e1.run()
    e2 = ReplayEngine(s2)
    e2.run()
    assert canonical_bytes(e1.trace_lines()) == \
        canonical_bytes(e2.trace_lines())


def test_trace_contains_required_fields():
    sample = generate_sample(seed=5, players=2, frames=6, disorder=1)
    engine = ReplayEngine(sample)
    engine.run()
    for line in engine.trace_lines():
        for key in ("frame", "applied", "state_hash", "quota_used",
                    "buffer_watermark", "events", "chain_hash"):
            assert key in line
