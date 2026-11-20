import pytest


def make_match(commands, frames=4, quota=5, window=2, players=("p1", "p2")):
    units = []
    for i, p in enumerate(players):
        units.append({"id": f"{p}-u0", "owner": p, "x": i * 10, "y": 0})
    return {
        "version": 1,
        "match_id": "test-match",
        "tick_ms": 100,
        "frames": frames,
        "compensation_window": window,
        "initial_state": {"players": list(players), "units": units},
        "quota_per_frame": {p: quota for p in players},
        "commands": commands,
    }


def cmd(player, seq, submit=0, recv=0, recv_tick=0, op="move",
        unit=None, dx=1, dy=0, cost=1):
    return {
        "player": player, "client_seq": seq, "submit_tick": submit,
        "recv_seq": recv, "recv_tick": recv_tick, "op": op,
        "unit": unit or f"{player}-u0", "dx": dx, "dy": dy, "cost": cost,
    }


@pytest.fixture()
def match_factory():
    return make_match
