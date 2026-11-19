from edge_sync.config import load_config
from edge_sync.rng import ByteStream, prf, uniform
from edge_sync.workload import build_manifest, payload_for, validate_envelope


def test_prf_is_pure_and_seeded():
    assert prf(1, 2, 3) == prf(1, 2, 3)
    assert prf(1, 2, 3) != prf(1, 2, 4)
    assert 0.0 <= uniform(7, 9) < 1.0


def test_manifest_is_deterministic(small_config):
    config = load_config(small_config)
    first = build_manifest(config)
    second = build_manifest(config)
    assert first == second
    payloads_equal = all(
        payload_for(config, shard) == payload_for(config, shard)
        for task in first
        for shard in task.shards
    )
    assert payloads_equal


def test_byte_stream_renders_exact_length_and_stable_bytes():
    stream = ByteStream(seed=5, task_id=1, shard_seq=2)
    payload = stream.render(5000)
    assert len(payload) == 5000
    again = ByteStream(seed=5, task_id=1, shard_seq=2).render(5000)
    assert payload == again


def test_resource_contract_enforced(small_config):
    config = load_config(small_config)
    manifest = build_manifest(config)
    assert validate_envelope(config, manifest) == []
    assert config.total_concurrency <= 16
    assert config.workload.task_concurrency <= 4


def test_shard_ranges_are_contiguous(small_config):
    config = load_config(small_config)
    manifest = build_manifest(config)
    for task in manifest:
        expected = 0
        for shard in task.shards:
            assert shard.start_byte == expected
            assert shard.end_byte == expected + shard.length
            expected = shard.end_byte
