// Package gen is the deterministic local sample generator. It constructs
// replays from a fixed seed and independently builds parser truth by
// construction bookkeeping; no network or external data is involved.
package gen

import (
	"os"
	"path/filepath"

	"replayaudit/internal/formats"
	"replayaudit/internal/model"
)

// FixedSeed makes every generated tree reproducible.
const FixedSeed int64 = 428691

// BuiltReplay bundles a normalized replay with its construction truth.
type BuiltReplay struct {
	Replay *model.Replay
	Truth  *model.Truth
}

// Index is the on-disk sample tree manifest.
type Index struct {
	Seed    int64        `json:"seed"`
	Params  model.Params `json:"params"`
	Replays []string     `json:"replays"`
}

// GenerateTree writes params.json, index.json and one subdirectory per replay
// containing format_1/2/3.json and truth.json.
func GenerateTree(root string) (*Index, error) {
	p := model.DefaultParams()
	all := BuildAll()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if err := formats.WriteJSON(filepath.Join(root, formats.FileParams), p); err != nil {
		return nil, err
	}
	idx := &Index{Seed: FixedSeed, Params: p}
	for i, b := range all {
		name := b.Replay.ReplayID
		idx.Replays = append(idx.Replays, name)
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := formats.WriteJSON(filepath.Join(dir, formats.FileParams), p); err != nil {
			return nil, err
		}
		f1, f2, f3 := ToFormats(b.Replay, p)
		if err := formats.WriteJSON(filepath.Join(dir, formats.File1), f1); err != nil {
			return nil, err
		}
		if err := formats.WriteJSON(filepath.Join(dir, formats.File2), f2); err != nil {
			return nil, err
		}
		if err := formats.WriteJSON(filepath.Join(dir, formats.File3), f3); err != nil {
			return nil, err
		}
		if err := formats.WriteJSON(filepath.Join(dir, formats.FileTruth), b.Truth); err != nil {
			return nil, err
		}
		_ = i
	}
	if err := formats.WriteJSON(filepath.Join(root, "index.json"), idx); err != nil {
		return nil, err
	}
	return idx, nil
}

// BuildAll returns the fixed scenario set plus one seeded pseudo-random replay.
func BuildAll() []*BuiltReplay {
	return []*BuiltReplay{
		scenarioBasic(),
		scenarioConditional(),
		scenarioMutex(),
		scenarioFull(),
		scenarioChainSequences(),
		scenarioSeeded(FixedSeed),
	}
}

// ---------------- construction truth builder ----------------

type builder struct {
	r      *model.Replay
	p      model.Params
	hp     map[string]int64
	mp     map[string]int64
	maxHP  map[string]int64
	maxMP  map[string]int64
	cd     map[string]int64
	effAt  map[string]int64
	status map[string]string
}

func newBuilder(id string, entities ...model.EntitySpec) *builder {
	b := &builder{
		r:      &model.Replay{ReplayID: id, Entities: entities},
		p:      model.DefaultParams(),
		hp:     map[string]int64{},
		mp:     map[string]int64{},
		maxHP:  map[string]int64{},
		maxMP:  map[string]int64{},
		cd:     map[string]int64{},
		effAt:  map[string]int64{},
		status: map[string]string{},
	}
	for _, e := range entities {
		b.maxHP[e.ID] = e.MaxHP
		b.maxMP[e.ID] = e.MaxMP
		hp := e.MaxHP
		if e.InitialHP > 0 {
			hp = e.InitialHP
		}
		mp := int64(0)
		if e.InitialMP > 0 {
			mp = e.InitialMP
		}
		b.hp[e.ID] = hp
		b.mp[e.ID] = mp
	}
	return b
}

func (b *builder) at(turn int) int64 { return int64(turn-1) * b.p.MsPerTurn }

type evOpts struct {
	turn      int
	kind      string
	amount    int64
	casters   []string
	targets   []string
	skill     string
	cd        int64
	condFlag  string
	condWant  bool
	hasCond   bool
	priority  int
	mandatory bool
}

func (b *builder) event(num int, o evOpts, effective bool, status string) *model.Event {
	id := formatID(num)
	e := &model.Event{
		ID: id, Kind: o.kind, At: b.at(o.turn),
		Casters: append([]string(nil), o.casters...),
		Targets: append([]string(nil), o.targets...),
		Amount:  o.amount, Skill: o.skill, CooldownUnits: o.cd,
		Priority: o.priority, Mandatory: o.mandatory,
	}
	if o.hasCond {
		e.Cond = &model.Cond{Flag: o.condFlag, Want: o.condWant}
	}
	b.r.Events = append(b.r.Events, e)
	b.effAt[id] = e.At
	b.status[id] = status
	if effective && status == "settled" {
		b.apply(e)
	}
	return e
}

func formatID(n int) string {
	const digits = "0123456789"
	s := ""
	for x := n; x > 0; x /= 10 {
		s = string(digits[x%10]) + s
	}
	for len(s) < 3 {
		s = "0" + s
	}
	return "e" + s
}

func (b *builder) apply(e *model.Event) {
	clamp := func(v, lo, hi int64) int64 {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	switch e.Kind {
	case "damage":
		for _, t := range e.Targets {
			b.hp[t] = clamp(b.hp[t]-e.Amount, 0, b.maxHP[t])
		}
	case "heal":
		for _, t := range e.Targets {
			b.hp[t] = clamp(b.hp[t]+e.Amount, 0, b.maxHP[t])
		}
	case "gain":
		for _, c := range e.Casters {
			b.mp[c] = clamp(b.mp[c]+e.Amount, 0, b.maxMP[c])
		}
	case "spend":
		for _, c := range e.Casters {
			b.mp[c] = clamp(b.mp[c]-e.Amount, 0, b.maxMP[c])
		}
	}
	if e.Skill != "" && e.CooldownUnits > 0 {
		for _, c := range e.Casters {
			b.cd[c+"|"+e.Skill] = e.At + e.CooldownUnits
		}
	}
}

func (b *builder) flag(id, name string, value bool, prio int) {
	b.r.Flags = append(b.r.Flags, model.FlagDef{ID: id, Name: name, Value: value, Priority: prio})
}

func (b *builder) mutex(id string, ids ...string) {
	b.r.Mutex = append(b.r.Mutex, model.MutexGroup{ID: id, EventIDs: ids})
}

func (b *builder) sync(turn int) {
	at := b.at(turn)
	b.r.Sync = append(b.r.Sync, model.SyncPoint{ID: formats.SyncID(at), At: at})
}

func (b *builder) build() *BuiltReplay {
	terminal := model.State{
		HP:       map[string]int64{},
		MP:       map[string]int64{},
		Cooldown: map[string]int64{},
	}
	for k, v := range b.hp {
		terminal.HP[k] = v
	}
	for k, v := range b.mp {
		terminal.MP[k] = v
	}
	for k, v := range b.cd {
		terminal.Cooldown[k] = v
	}
	winner := genWinner(b.r.Entities, b.hp)
	terminal.Winner = winner
	terminal.Ended = winner != ""
	syncTimes := model.SortedSyncTimes(b.r.Sync)
	truth := &model.Truth{
		ReplayID: b.r.ReplayID, EffectiveAt: b.effAt, Status: b.status,
		Terminal: terminal, SyncPoints: syncTimes, Winner: winner,
	}
	return &BuiltReplay{Replay: b.r, Truth: truth}
}

func genWinner(entities []model.EntitySpec, hp map[string]int64) string {
	sides := map[string][]string{}
	for _, e := range entities {
		sides[e.Side] = append(sides[e.Side], e.ID)
	}
	if len(sides) != 2 {
		return ""
	}
	names := make([]string, 0, 2)
	for n := range sides {
		names = append(names, n)
	}
	// deterministic side order
	if names[0] > names[1] {
		names[0], names[1] = names[1], names[0]
	}
	dead := func(s string) bool {
		for _, id := range sides[s] {
			if hp[id] > 0 {
				return false
			}
		}
		return true
	}
	a, z := names[0], names[1]
	switch {
	case dead(a) && dead(z):
		return "draw"
	case dead(a):
		return z
	case dead(z):
		return a
	default:
		return ""
	}
}

// helper for pointer bools in generator scenarios
func boolean(v bool) *bool { return &v }
