import copy

from syncdemo import engine, runner
from syncdemo.client import ClientSim


def _units():
    return [
        {"id": "u0", "player": "p0", "x": 10.0, "y": 10.0, "facing": 0.0,
         "hp": 100.0, "max_hp": 100.0, "resource": 0.0, "cooldown": 0,
         "speed": 2.5},
        {"id": "u1", "player": "p1", "x": 20.0, "y": 20.0, "facing": 0.0,
         "hp": 100.0, "max_hp": 100.0, "resource": 0.0, "cooldown": 0,
         "speed": 2.5},
    ]


def test_rollback_adopts_authoritative_state_exactly():
    cfg = engine.EngineConfig()
    client = ClientSim("c0", "p0", seed=3, cfg=cfg)
    auth = engine.initial_state(_units())
    client.on_authoritative(auth)
    assert client.confirmed == auth

    # local prediction diverges from what the server will compute
    client.apply_local_input(1, {"unit": "u0", "type": "move",
                                 "dx": 2.0, "dy": 0.0})
    predicted = copy.deepcopy(client.visible_state())
    assert predicted["units"]["u0"]["x"] != auth["units"]["u0"]["x"]

    # authoritative tick 1 arrives with a *different* intent applied
    engine.step(auth, [{"unit": "u0", "type": "gather"}], seed=3, cfg=cfg)
    client.on_authoritative(auth)
    assert client.confirmed == auth
    assert client.rollback_count == 2
    # correction recorded: predicted u0 moved, authoritative u0 gathered
    last = client.corrections[-1]
    assert last.tick == 1
    assert last.total > 0.0
    assert "u0" in last.magnitude

    # unconfirmed input replayed on top of the confirmed state
    client.apply_local_input(2, {"unit": "u0", "type": "move",
                                 "dx": 1.0, "dy": 0.0})
    view = client.visible_state()
    assert view["tick"] == 2
    assert view["units"]["u0"]["x"] != client.confirmed["units"]["u0"]["x"]


def test_confirmed_matches_authoritative_field_by_field(sample):
    result = runner.run_replay(sample)
    for cid, corrections in result.corrections.items():
        assert corrections, "expected rollback events on every client"
    # after the latency drain every client sees the authoritative state
    for cid, views in result.client_views.items():
        final_view = views[-1]
        assert final_view["tick"] == result.states[-1]["tick"]
        for uid, unit in result.states[-1]["units"].items():
            seen = final_view["units"][uid]
            for fname in engine.COMPARE_FIELDS:
                assert seen[fname] == unit[fname], (cid, uid, fname)


def test_client_visible_matches_settlement(sample):
    result = runner.run_replay(sample)
    settlement = result.settlement
    for cid, views in result.client_views.items():
        final = views[-1]
        players = {}
        for unit in final["units"].values():
            entry = players.setdefault(unit["player"],
                                       {"resource": 0.0, "hp": 0.0})
            entry["resource"] += unit["resource"]
            entry["hp"] += unit["hp"]
        assert players == settlement["players"]
