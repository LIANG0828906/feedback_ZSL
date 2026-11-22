import copy

from syncdemo import engine
from syncdemo.rng import derive_rng


def _units():
    return [
        {"id": "u0", "player": "p0", "x": 10.0, "y": 10.0, "facing": 0.0,
         "hp": 100.0, "max_hp": 100.0, "resource": 50.0, "cooldown": 0,
         "speed": 2.5},
        {"id": "u1", "player": "p1", "x": 20.0, "y": 20.0, "facing": 0.0,
         "hp": 30.0, "max_hp": 100.0, "resource": 0.0, "cooldown": 0,
         "speed": 2.5},
    ]


def test_deterministic_runs():
    intents = [[{"unit": "u0", "type": "skill", "target": "u1"},
                {"unit": "u1", "type": "gather"}] for _ in range(5)]
    states = []
    for _ in range(2):
        state = engine.initial_state(_units())
        for tick_intents in intents:
            engine.step(state, tick_intents, seed=7, cfg=engine.EngineConfig())
        states.append(copy.deepcopy(state))
    assert states[0] == states[1]


def test_rng_derivation_path_independent():
    first = derive_rng(42, 9, "tick").integers(0, 1000)
    derive_rng(42, 1, "tick").integers(0, 1000)  # consume elsewhere
    derive_rng(42, 8, "tick").random()
    again = derive_rng(42, 9, "tick").integers(0, 1000)
    assert first == again
    seq_a = derive_rng(42, 9, "tick").random(4)
    seq_b = derive_rng(42, 10, "tick").random(4)
    assert not (seq_a == seq_b).all()


def test_skill_cost_cooldown_and_damage():
    cfg = engine.EngineConfig()
    state = engine.initial_state(_units())
    engine.step(state, [{"unit": "u0", "type": "skill", "target": "u1"}],
                seed=1, cfg=cfg)
    u0, u1 = state["units"]["u0"], state["units"]["u1"]
    assert u0["resource"] == 50.0 - cfg.skill_cost
    assert u0["cooldown"] == cfg.skill_cooldown - 1  # decremented end of tick
    assert u1["hp"] < 30.0


def test_skill_prerequisite_blocks_cast():
    cfg = engine.EngineConfig()
    state = engine.initial_state(_units())
    state["units"]["u0"]["resource"] = 0.0
    hp_before = state["units"]["u1"]["hp"]
    engine.step(state, [{"unit": "u0", "type": "skill", "target": "u1"}],
                seed=1, cfg=cfg)
    assert state["units"]["u1"]["hp"] == hp_before


def test_death_and_respawn():
    cfg = engine.EngineConfig()
    state = engine.initial_state(_units())
    state["units"]["u1"]["hp"] = 1.0
    engine.step(state, [{"unit": "u0", "type": "skill", "target": "u1"}],
                seed=1, cfg=cfg)
    u1 = state["units"]["u1"]
    assert not u1["alive"]
    assert u1["respawn_tick"] == 1 + cfg.respawn_delay
    for _ in range(cfg.respawn_delay):
        engine.step(state, [], seed=1, cfg=cfg)
    assert u1["alive"]
    assert u1["hp"] == u1["max_hp"]
    assert (u1["x"], u1["y"]) == (u1["spawn_x"], u1["spawn_y"])


def test_move_clamped_to_speed_and_bounds():
    cfg = engine.EngineConfig()
    state = engine.initial_state(_units())
    engine.step(state, [{"unit": "u0", "type": "move", "dx": 100.0, "dy": 0.0}],
                seed=1, cfg=cfg)
    u0 = state["units"]["u0"]
    assert u0["x"] == 10.0 + 2.5
    assert u0["facing"] == 0.0
    state["units"]["u0"]["y"] = 1.0
    engine.step(state,
                [{"unit": "u0", "type": "move", "dx": 0.0, "dy": -999.0}],
                seed=1, cfg=cfg)
    assert state["units"]["u0"]["y"] == 0.0


def test_lock_and_settlement():
    cfg = engine.EngineConfig()
    state = engine.initial_state(_units())
    state["units"]["u0"]["resource"] = cfg.resource_target
    engine.step(state, [], seed=1, cfg=cfg)
    assert state["locked"]
    assert state["result"] is not None
    frozen = copy.deepcopy(state)
    engine.step(state, [{"unit": "u0", "type": "gather"}], seed=1, cfg=cfg)
    assert state == frozen
    result = state["result"]
    assert result["players"]["p0"]["resource"] >= cfg.resource_target
    assert result["winner"] == "p0"
