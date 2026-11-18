import time

import pytest

from orderlab_gateway.errors import E_CONN, GatewayError
from orderlab_gateway.limiter import FairLimiter


def test_global_connection_cap_and_bounded_wait():
    limiter = FairLimiter(2, 1, 50)
    limiter.acquire("a")
    limiter.acquire("b")
    started = time.monotonic()
    with pytest.raises(GatewayError) as caught:
        limiter.acquire("c")
    elapsed = time.monotonic() - started
    assert caught.value.code == E_CONN
    assert caught.value.details["reason"] == "global_limit"
    assert elapsed < 0.5
    assert limiter.status()["active"] == 2
    limiter.release("a")
    limiter.release("b")
    assert limiter.status()["active"] == 0


def test_one_tenant_cannot_take_all_connections():
    limiter = FairLimiter(4, 2, 50)
    limiter.acquire("loud")
    limiter.acquire("loud")
    with pytest.raises(GatewayError) as caught:
        limiter.acquire("loud")
    assert caught.value.details["reason"] == "tenant_limit"
    assert limiter.status()["by_tenant"] == {"loud": 2}
    limiter.release("loud")
    limiter.release("loud")
