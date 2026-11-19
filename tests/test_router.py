"""Stable routing, live weight changes, fallback and client audit traces."""

from gateway.router import Router


def make_router(weight=0.3):
    return Router("v3", "v4", weight, tenant_defaults={"tenant-b": 0.9})


def test_weight_zero_routes_everything_to_stable():
    router = make_router(weight=0.0)
    for index in range(20):
        decision = router.route("tenant-a", f"c{index}", f"r{index}", True)
        assert decision["target_version"] == "v3"


def test_weight_one_routes_everything_to_canary_when_available():
    router = make_router(weight=1.0)
    targets = {
        router.route("tenant-a", "c1", f"r{i}", True)["target_version"]
        for i in range(10)
    }
    assert targets == {"v4"}


def test_unavailable_canary_falls_back_to_stable():
    router = make_router(weight=1.0)
    decision = router.route("tenant-a", "c1", "r1", canary_available=False)
    assert decision["target_version"] == "v3"
    assert decision["reason"] == "canary_unavailable_fallback"


def test_weight_change_applies_to_subsequent_requests_and_is_traceable():
    router = make_router(weight=0.0)
    before = router.route("tenant-a", "client-x", "r-before", True)
    router.set_default_weight(1.0)
    after = router.route("tenant-a", "client-x", "r-after", True)
    assert before["target_version"] == "v3"
    assert after["target_version"] == "v4"
    trace = router.client_trace("client-x")
    assert [item["request_id"] for item in trace] == ["r-before", "r-after"]
    assert trace[0]["weight"] == 0.0
    assert trace[1]["weight"] == 1.0


def test_tenant_default_weight_used_without_client_id():
    router = make_router(weight=0.0)
    decision = router.route("tenant-b", None, "r-anon", True)
    assert decision["weight"] == 0.9


def test_routing_is_deterministic_for_same_identity():
    router = make_router(weight=0.5)
    first = router.route("tenant-a", "c", "r", True)
    second = router.route("tenant-a", "c", "r", True)
    assert first["bucket"] == second["bucket"]
    assert first["target_version"] == second["target_version"]


def test_zero_canary_converges_all_weights():
    router = make_router(weight=0.3)
    router.zero_canary()
    assert router.get_weight("tenant-a") == 0.0
    assert router.get_weight("tenant-b") == 0.0
