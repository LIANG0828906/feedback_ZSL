package gen

import (
	"math/rand"

	"replayaudit/internal/model"
)

func stdEntities() []model.EntitySpec {
	return []model.EntitySpec{
		{ID: "a1", Side: "blue", MaxHP: 100, MaxMP: 100, InitialMP: 40},
		{ID: "a2", Side: "blue", MaxHP: 90, MaxMP: 60, InitialMP: 60},
		{ID: "b1", Side: "red", MaxHP: 120, MaxMP: 50},
		{ID: "b2", Side: "red", MaxHP: 80, MaxMP: 80},
	}
}

func scenarioBasic() *BuiltReplay {
	b := newBuilder("r01_basic", stdEntities()...)
	n := 0
	next := func() int { n++; return n }

	b.event(next(), evOpts{turn: 1, kind: "damage", amount: 20, casters: []string{"a1"}, targets: []string{"b1"}, skill: "strike", cd: 3000}, true, "settled")
	b.event(next(), evOpts{turn: 2, kind: "heal", amount: 15, casters: []string{"b2"}, targets: []string{"b1"}}, true, "settled")
	b.event(next(), evOpts{turn: 2, kind: "gain", amount: 30, casters: []string{"a1"}, targets: []string{"a1"}}, true, "settled")
	b.event(next(), evOpts{turn: 3, kind: "spend", amount: 25, casters: []string{"a2"}, targets: []string{"a1"}}, true, "settled")
	b.event(next(), evOpts{turn: 4, kind: "damage", amount: 45, casters: []string{"a1"}, targets: []string{"b2"}, skill: "fire", cd: 6000}, true, "settled")
	b.sync(4)
	b.event(next(), evOpts{turn: 5, kind: "heal", amount: 20, casters: []string{"b2"}, targets: []string{"b2"}}, true, "settled")
	b.event(next(), evOpts{turn: 6, kind: "damage", amount: 40, casters: []string{"b1"}, targets: []string{"a2"}}, true, "settled")
	b.event(next(), evOpts{turn: 7, kind: "damage", amount: 200, casters: []string{"a1"}, targets: []string{"b1"}, skill: "execute", cd: 9000}, true, "settled")
	b.event(next(), evOpts{turn: 8, kind: "damage", amount: 200, casters: []string{"a2"}, targets: []string{"b2"}}, true, "settled")
	b.sync(8)
	return b.build()
}

func scenarioConditional() *BuiltReplay {
	b := newBuilder("r02_conditional", stdEntities()...)
	b.flag("flagdef:ult_unlocked", "ult_unlocked", true, 0)
	b.flag("flagdef:weather_rain", "weather_rain", false, 0)
	n := 0
	next := func() int { n++; return n }

	b.event(next(), evOpts{turn: 1, kind: "damage", amount: 10, casters: []string{"a1"}, targets: []string{"b1"}}, true, "settled")
	b.event(next(), evOpts{turn: 2, kind: "damage", amount: 50, casters: []string{"a1"}, targets: []string{"b1"},
		hasCond: true, condFlag: "ult_unlocked", condWant: true}, true, "settled")
	b.event(next(), evOpts{turn: 3, kind: "damage", amount: 999, casters: []string{"a2"}, targets: []string{"b1"},
		hasCond: true, condFlag: "weather_rain", condWant: true}, false, "skipped_cond")
	b.event(next(), evOpts{turn: 4, kind: "heal", amount: 30, casters: []string{"b2"}, targets: []string{"b1"},
		hasCond: true, condFlag: "ult_unlocked", condWant: false}, false, "skipped_cond")
	b.event(next(), evOpts{turn: 5, kind: "gain", amount: 25, casters: []string{"a2"}, targets: []string{"a2"},
		hasCond: true, condFlag: "weather_rain", condWant: false}, true, "settled")
	b.sync(5)
	b.event(next(), evOpts{turn: 6, kind: "spend", amount: 10, casters: []string{"a1"}, targets: []string{"a1"},
		hasCond: true, condFlag: "ult_unlocked", condWant: true}, true, "settled")
	b.event(next(), evOpts{turn: 7, kind: "damage", amount: 200, casters: []string{"a1"}, targets: []string{"b1"}}, true, "settled")
	b.event(next(), evOpts{turn: 8, kind: "damage", amount: 200, casters: []string{"a2"}, targets: []string{"b2"}}, true, "settled")
	return b.build()
}

func scenarioMutex() *BuiltReplay {
	b := newBuilder("r03_mutex", stdEntities()...)
	n := 0
	next := func() int { n++; return n }

	b.event(next(), evOpts{turn: 1, kind: "damage", amount: 10, casters: []string{"a1"}, targets: []string{"b1"}}, true, "settled")
	b.event(next(), evOpts{turn: 2, kind: "damage", amount: 10, casters: []string{"a2"}, targets: []string{"b2"}}, true, "settled")
	win := b.event(next(), evOpts{turn: 4, kind: "damage", amount: 50, casters: []string{"a1"}, targets: []string{"b1"}, priority: 10}, true, "settled")
	lose := b.event(next(), evOpts{turn: 4, kind: "damage", amount: 50, casters: []string{"a2"}, targets: []string{"b1"}, priority: 1}, false, "suppressed")
	b.mutex("g_duel", win.ID, lose.ID)

	hwin := b.event(next(), evOpts{turn: 6, kind: "heal", amount: 10, casters: []string{"b2"}, targets: []string{"b1"}, priority: 0}, true, "settled")
	hlose := b.event(next(), evOpts{turn: 6, kind: "heal", amount: 999, casters: []string{"b2"}, targets: []string{"b1"}, priority: 0}, false, "suppressed")
	b.mutex("g_heal", hwin.ID, hlose.ID)
	b.sync(6)

	b.event(next(), evOpts{turn: 8, kind: "damage", amount: 200, casters: []string{"a1"}, targets: []string{"b1"}}, true, "settled")
	b.event(next(), evOpts{turn: 9, kind: "damage", amount: 200, casters: []string{"a2"}, targets: []string{"b2"}}, true, "settled")
	return b.build()
}

func scenarioFull() *BuiltReplay {
	ents := []model.EntitySpec{
		{ID: "p1", Side: "blue", MaxHP: 110, MaxMP: 100, InitialMP: 50},
		{ID: "p2", Side: "blue", MaxHP: 100, MaxMP: 80, InitialMP: 30},
		{ID: "q1", Side: "red", MaxHP: 130, MaxMP: 60},
		{ID: "q2", Side: "red", MaxHP: 90, MaxMP: 60},
	}
	b := newBuilder("r04_full", ents...)
	b.flag("flagdef:enraged", "enraged", true, 0)
	n := 0
	next := func() int { n++; return n }

	b.event(next(), evOpts{turn: 1, kind: "damage", amount: 15, casters: []string{"p1"}, targets: []string{"q1"}, skill: "slash", cd: 3000}, true, "settled")
	b.event(next(), evOpts{turn: 1, kind: "damage", amount: 15, casters: []string{"p2"}, targets: []string{"q2"}}, true, "settled")
	b.sync(2)
	b.event(next(), evOpts{turn: 3, kind: "damage", amount: 40, casters: []string{"p1"}, targets: []string{"q1"},
		hasCond: true, condFlag: "enraged", condWant: true}, true, "settled")
	b.event(next(), evOpts{turn: 3, kind: "damage", amount: 40, casters: []string{"p2"}, targets: []string{"q2"},
		hasCond: true, condFlag: "enraged", condWant: false}, false, "skipped_cond")
	w := b.event(next(), evOpts{turn: 5, kind: "damage", amount: 30, casters: []string{"p1"}, targets: []string{"q1"}, priority: 9}, true, "settled")
	l := b.event(next(), evOpts{turn: 5, kind: "damage", amount: 30, casters: []string{"p2"}, targets: []string{"q1"}, priority: 2}, false, "suppressed")
	b.mutex("g_focus", w.ID, l.ID)
	b.sync(6)
	b.event(next(), evOpts{turn: 7, kind: "gain", amount: 20, casters: []string{"p1"}, targets: []string{"p2"}}, true, "settled")
	b.event(next(), evOpts{turn: 7, kind: "damage", amount: 300, casters: []string{"p1"}, targets: []string{"q1"}}, true, "settled")
	b.event(next(), evOpts{turn: 8, kind: "damage", amount: 300, casters: []string{"p2"}, targets: []string{"q2"}}, true, "settled")
	b.sync(8)
	return b.build()
}

func scenarioChainSequences() *BuiltReplay {
	b := newBuilder("r05_chain", []model.EntitySpec{
		{ID: "a1", Side: "blue", MaxHP: 100, MaxMP: 100, InitialMP: 50},
		{ID: "a2", Side: "blue", MaxHP: 100, MaxMP: 100, InitialMP: 50},
		{ID: "b1", Side: "red", MaxHP: 100, MaxMP: 100},
		{ID: "b2", Side: "red", MaxHP: 100, MaxMP: 100},
	}...)
	n := 0
	next := func() int { n++; return n }

	b.event(next(), evOpts{turn: 1, kind: "damage", amount: 10,
		casters: []string{"a1", "a2"}, targets: []string{"b1", "b2"}}, true, "settled")
	b.event(next(), evOpts{turn: 2, kind: "gain", amount: 20,
		casters: []string{"a1", "a2"}, targets: []string{"a1", "a2"}}, true, "settled")
	b.event(next(), evOpts{turn: 3, kind: "spend", amount: 15,
		casters: []string{"a1", "a2"}, targets: []string{"a1", "a2"}}, true, "settled")
	b.event(next(), evOpts{turn: 4, kind: "heal", amount: 5,
		casters: []string{"b1", "b2"}, targets: []string{"b1", "b2"}}, true, "settled")
	b.sync(4)
	b.event(next(), evOpts{turn: 6, kind: "damage", amount: 300,
		casters: []string{"a1", "a2"}, targets: []string{"b1", "b2"}}, true, "settled")
	return b.build()
}

func scenarioSeeded(seed int64) *BuiltReplay {
	rng := rand.New(rand.NewSource(seed + 7))
	ents := []model.EntitySpec{
		{ID: "f1", Side: "blue", MaxHP: 120, MaxMP: 120, InitialMP: 60},
		{ID: "f2", Side: "blue", MaxHP: 100, MaxMP: 100, InitialMP: 40},
		{ID: "g1", Side: "red", MaxHP: 140, MaxMP: 80},
		{ID: "g2", Side: "red", MaxHP: 90, MaxMP: 80},
	}
	b := newBuilder("r06_seeded", ents...)
	flagVal := rng.Intn(2) == 1
	b.flag("flagdef:fuzzflag", "fuzzflag", flagVal, 0)

	blue := []string{"f1", "f2"}
	red := []string{"g1", "g2"}
	n := 0
	next := func() int { n++; return n }

	for turn := 1; turn <= 9; turn++ {
		if turn%3 == 0 {
			b.sync(turn)
		}
		count := 1 + rng.Intn(2)
		for k := 0; k < count; k++ {
			kind := []string{"damage", "heal", "gain", "spend"}[rng.Intn(4)]
			amount := int64(5 + rng.Intn(25))
			o := evOpts{turn: turn, kind: kind, amount: amount}
			switch kind {
			case "damage":
				o.casters = []string{blue[rng.Intn(len(blue))]}
				o.targets = []string{red[rng.Intn(len(red))]}
			case "heal":
				o.casters = []string{red[rng.Intn(len(red))]}
				o.targets = []string{red[rng.Intn(len(red))]}
			case "gain", "spend":
				o.casters = []string{blue[rng.Intn(len(blue))]}
				o.targets = []string{o.casters[0]}
			}
			if rng.Intn(3) == 0 {
				o.hasCond = true
				o.condFlag = "fuzzflag"
				o.condWant = rng.Intn(2) == 1
				effective := o.condWant == flagVal
				if effective {
					b.event(next(), o, true, "settled")
				} else {
					b.event(next(), o, false, "skipped_cond")
				}
			} else {
				b.event(next(), o, true, "settled")
			}
		}
	}
	// Deterministic finishers guarantee a winner.
	b.event(next(), evOpts{turn: 11, kind: "damage", amount: 1000, casters: []string{"f1"}, targets: []string{"g1"}}, true, "settled")
	b.event(next(), evOpts{turn: 12, kind: "damage", amount: 1000, casters: []string{"f2"}, targets: []string{"g2"}}, true, "settled")
	b.sync(12)
	return b.build()
}
