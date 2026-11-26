package incremental

import (
	"encoding/json"
	"fmt"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/engine"
	"mirrorcast/internal/session"
)

type EditKind string

const (
	EditScale       EditKind = "scale"
	EditChroma      EditKind = "chroma"
	EditBudget      EditKind = "budget"
	EditAddEvent    EditKind = "add_event"
	EditDeleteEvent EditKind = "delete_event"
	EditUpdateEvent EditKind = "update_event"
)

type Edit struct {
	Kind     EditKind
	AtTick   int64
	ScreenID string
	Scale    int64
	Chroma   session.ChromaFormat
	Budget   int64
	EventID  string
	Event    *session.Event
}

type Provenance struct {
	Tick     int64    `json:"tick"`
	Scope    string   `json:"scope"`
	ScreenID string   `json:"screen_id,omitempty"`
	Node     string   `json:"node"`
	Status   string   `json:"status"`
	CacheKey string   `json:"cache_key,omitempty"`
	Sources  []string `json:"sources"`
}

type Report struct {
	EditSource      string               `json:"edit_source"`
	DirtyFromTick   int64                `json:"dirty_from_tick"`
	Results         []session.TickResult `json:"results"`
	Provenance      []Provenance         `json:"provenance"`
	RecomputedTicks []int64              `json:"recomputed_ticks"`
	ReusedTicks     []int64              `json:"reused_ticks"`
	CacheHits       int64                `json:"cache_hits"`
	CacheMisses     int64                `json:"cache_misses"`
}

type Evaluator struct {
	project session.Project
	cache   *engine.MappingCache
	results []session.TickResult
	states  map[int64]engine.State
	maxTick int64
}

func NewEvaluator(project session.Project) (*Evaluator, error) {
	clone, err := cloneProject(project)
	if err != nil {
		return nil, err
	}
	return &Evaluator{project: clone, cache: engine.NewMappingCache(), states: map[int64]engine.State{}}, nil
}

func (e *Evaluator) Project() session.Project { return e.project }

func (e *Evaluator) Baseline() (*Report, error) {
	eng, err := engine.NewWithCache(e.project, e.cache)
	if err != nil {
		return nil, err
	}
	e.states = map[int64]engine.State{0: eng.State()}
	e.results = nil
	for tick := int64(1); tick <= eng.MaxTick(); tick++ {
		result, err := eng.AdvanceOne()
		if err != nil {
			return nil, err
		}
		e.results = append(e.results, result)
		e.states[tick] = eng.State()
	}
	e.maxTick = eng.MaxTick()

	report := &Report{EditSource: "baseline", DirtyFromTick: 1, Results: e.results, CacheMisses: e.cache.Misses, CacheHits: e.cache.Hits}
	for _, tick := range e.results {
		report.RecomputedTicks = append(report.RecomputedTicks, tick.Tick)
		report.Provenance = append(report.Provenance, Provenance{
			Tick: tick.Tick, Scope: "tick", Node: "tick_result", Status: "computed",
			Sources: []string{"baseline:deterministic_generator"},
		})
	}
	return report, nil
}

func (e *Evaluator) ApplyEdit(edit Edit) (*Report, error) {
	knownKeys := e.cache.Keys()
	hitsBefore, missesBefore := e.cache.Hits, e.cache.Misses
	source, dirtyTick, candidate, err := e.buildCandidate(edit)
	if err != nil {
		return nil, err
	}

	fork, err := engine.NewWithCache(candidate, e.cache)
	if err != nil {
		return nil, err
	}
	baseState, ok := e.states[dirtyTick-1]
	if !ok {
		var latest int64
		for tick := range e.states {
			if tick < dirtyTick && tick > latest {
				latest = tick
			}
		}
		baseState, ok = e.states[latest]
		if !ok {
			return nil, bizerror.New(bizerror.EInvalidEdit, "no committable state before edit tick").WithDetail("dirty_tick", dirtyTick)
		}
	}
	if err := fork.RestoreState(baseState); err != nil {
		return nil, err
	}

	var gapResults []session.TickResult
	for fork.State().CommittedTick < dirtyTick-1 {
		result, err := fork.AdvanceOne()
		if err != nil {
			return nil, err
		}
		gapResults = append(gapResults, result)
		e.states[result.Tick] = fork.State()
	}

	var suffix []session.TickResult
	for fork.State().CommittedTick < fork.MaxTick() {
		result, err := fork.AdvanceOne()
		if err != nil {
			return nil, err
		}
		suffix = append(suffix, result)
		e.states[result.Tick] = fork.State()
	}
	newMax := fork.MaxTick()

	byTick := map[int64]session.TickResult{}
	for _, result := range e.results {
		if result.Tick < dirtyTick {
			byTick[result.Tick] = result
		}
	}
	for _, result := range gapResults {
		byTick[result.Tick] = result
	}
	for _, result := range suffix {
		byTick[result.Tick] = result
	}
	ordered := make([]session.TickResult, 0, newMax)
	for tick := int64(1); tick <= newMax; tick++ {
		if result, ok := byTick[tick]; ok {
			ordered = append(ordered, result)
		}
	}
	e.results = ordered
	e.maxTick = newMax
	e.project = candidate

	report := &Report{
		EditSource:    source,
		DirtyFromTick: dirtyTick,
		Results:       ordered,
		CacheHits:     e.cache.Hits - hitsBefore,
		CacheMisses:   e.cache.Misses - missesBefore,
	}
	for tick := int64(1); tick < dirtyTick && tick <= e.maxTick; tick++ {
		if result, ok := byTick[tick]; ok {
			report.ReusedTicks = append(report.ReusedTicks, tick)
			report.Provenance = append(report.Provenance, Provenance{
				Tick: tick, Scope: "tick", Node: "tick_result", Status: "reused",
				Sources: []string{fmt.Sprintf("cache:tick-result:%d", tick), "invalidation_boundary:edit_at_tick_" + fmt.Sprint(dirtyTick)},
			})
			for _, screen := range result.Results {
				report.Provenance = append(report.Provenance, Provenance{
					Tick: tick, Scope: "screen_result", ScreenID: screen.ScreenID, Node: "complete_screen_result", Status: "reused",
					Sources: []string{fmt.Sprintf("cache:tick-result:%d#%s", tick, screen.ScreenID)},
				})
			}
		}
	}

	for _, result := range suffix {
		report.RecomputedTicks = append(report.RecomputedTicks, result.Tick)
		report.Provenance = append(report.Provenance, Provenance{
			Tick: result.Tick, Scope: "tick", Node: "budget_and_topology_settlement", Status: "recomputed",
			Sources: []string{source, "dependency:shared_budget+C-BUDGET-TOTAL"},
		})
		for _, screen := range result.Results {
			entry := Provenance{
				Tick: result.Tick, Scope: "screen_result", ScreenID: screen.ScreenID,
				Node: "mirror_region_and_admission", Status: "recomputed", Sources: []string{source},
			}
			if screen.Mapping != nil {
				key := engine.MappingKey(e.project.Master, lookupWidth(e.project, screen.ScreenID), lookupHeight(e.project, screen.ScreenID), screen.SettledScale, screen.Mapping.Chroma)
				entry.CacheKey = key
				if _, existed := knownKeys[key]; existed && screen.ScreenID != edit.ScreenID {
					entry.Status = "geometry_cache_reused_settlement_recomputed"
					entry.Sources = []string{fmt.Sprintf("cache:geometry:%s", key), "dependency:budget_settlement"}
				} else if _, existed := knownKeys[key]; existed && edit.Kind != EditScale && edit.Kind != EditChroma {
					entry.Status = "geometry_cache_reused_settlement_recomputed"
					entry.Sources = []string{fmt.Sprintf("cache:geometry:%s", key), "dependency:event_chain"}
				} else {
					entry.Status = "geometry_recomputed"
					entry.Sources = []string{source, "miss:geometry_cache"}
				}
			}
			report.Provenance = append(report.Provenance, entry)
		}
	}
	return report, nil
}

func (e *Evaluator) buildCandidate(edit Edit) (string, int64, session.Project, error) {
	candidate, err := cloneProject(e.project)
	if err != nil {
		return "", 0, session.Project{}, err
	}
	dirty := edit.AtTick
	source := "edit:" + string(edit.Kind)

	switch edit.Kind {
	case EditScale, EditChroma:
		if edit.ScreenID == "" {
			return "", 0, session.Project{}, bizerror.New(bizerror.EInvalidEdit, "screen edit requires screen_id")
		}
		idx := findScreen(candidate.Screens, edit.ScreenID)
		if idx < 0 {
			return "", 0, session.Project{}, bizerror.New(bizerror.EScreenNotFound, "edit references unknown screen").WithScreen(edit.ScreenID)
		}
		if dirty <= 0 {
			dirty = 1
		}
		update := &session.ScreenUpdate{}
		if edit.Kind == EditScale {
			value := edit.Scale
			candidate.Screens[idx].ScalePermyriad = value
			update.ScalePermyriad = &value
			source += ":" + edit.ScreenID + ":scale_permyriad=" + fmt.Sprint(value)
		} else {
			value := edit.Chroma
			candidate.Screens[idx].Chroma = value
			update.Chroma = &value
			source += ":" + edit.ScreenID + ":chroma=" + string(value)
		}
		eventID := fmt.Sprintf("incremental-%s-%s-t%d", edit.Kind, edit.ScreenID, dirty)
		candidate.Events = append(candidate.Events, session.Event{
			ID: eventID, Tick: dirty, Type: session.EventUpdate, ScreenID: edit.ScreenID, Update: update,
		})

	case EditBudget:
		if dirty <= 0 {
			dirty = 1
		}
		source += ":total_budget=" + fmt.Sprint(edit.Budget) + "@tick" + fmt.Sprint(dirty)
		replaced := false
		for i := range candidate.Events {
			if candidate.Events[i].Type == session.EventBudget && candidate.Events[i].Tick == dirty {
				candidate.Events[i].Budget = edit.Budget
				replaced = true
				break
			}
		}
		if !replaced {
			candidate.Events = append(candidate.Events, session.Event{
				ID: fmt.Sprintf("incremental-budget-t%d", dirty), Tick: dirty,
				Type: session.EventBudget, Budget: edit.Budget,
			})
		}

	case EditAddEvent:
		if edit.Event == nil {
			return "", 0, session.Project{}, bizerror.New(bizerror.EInvalidEdit, "add_event requires event")
		}
		event := *edit.Event
		if event.ID == "" {
			event.ID = fmt.Sprintf("incremental-event-t%d-%s", event.Tick, event.Type)
		}
		if event.Tick <= 0 {
			return "", 0, session.Project{}, bizerror.New(bizerror.ETopologyEventInvalid, "event tick must be positive").WithDetail("event_id", event.ID)
		}
		dirty = event.Tick
		source += ":" + event.ID + "@tick" + fmt.Sprint(dirty)
		candidate.Events = append(candidate.Events, event)

	case EditDeleteEvent:
		idx := findEvent(candidate.Events, edit.EventID)
		if idx < 0 {
			return "", 0, session.Project{}, bizerror.New(bizerror.EInvalidEdit, "delete references unknown event").WithDetail("event_id", edit.EventID)
		}
		dirty = candidate.Events[idx].Tick
		source += ":" + edit.EventID + "@tick" + fmt.Sprint(dirty) + ":deleted"
		candidate.Events = append(candidate.Events[:idx], candidate.Events[idx+1:]...)

	case EditUpdateEvent:
		if edit.Event == nil || edit.EventID == "" {
			return "", 0, session.Project{}, bizerror.New(bizerror.EInvalidEdit, "update_event requires event id and replacement")
		}
		idx := findEvent(candidate.Events, edit.EventID)
		if idx < 0 {
			return "", 0, session.Project{}, bizerror.New(bizerror.EInvalidEdit, "update references unknown event").WithDetail("event_id", edit.EventID)
		}
		oldTick := candidate.Events[idx].Tick
		replacement := *edit.Event
		if replacement.ID == "" {
			replacement.ID = edit.EventID
		}
		candidate.Events[idx] = replacement
		dirty = oldTick
		if replacement.Tick < dirty {
			dirty = replacement.Tick
		}
		source += ":" + edit.EventID + "@tick" + fmt.Sprint(dirty) + ":updated"

	default:
		return "", 0, session.Project{}, bizerror.New(bizerror.EInvalidEdit, "unknown incremental edit kind").WithDetail("kind", edit.Kind)
	}
	return source, dirty, candidate, nil
}

func cloneProject(project session.Project) (session.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return session.Project{}, bizerror.New(bizerror.EInternal, "cannot clone project: "+err.Error())
	}
	var out session.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return session.Project{}, bizerror.New(bizerror.EInternal, "cannot clone project: "+err.Error())
	}
	return out, nil
}

func findScreen(screens []session.Screen, id string) int {
	for i := range screens {
		if screens[i].ID == id {
			return i
		}
	}
	return -1
}

func findEvent(events []session.Event, id string) int {
	for i := range events {
		if events[i].ID == id {
			return i
		}
	}
	return -1
}

func lookupWidth(project session.Project, id string) int64 {
	if idx := findScreen(project.Screens, id); idx >= 0 {
		return project.Screens[idx].Width
	}
	return 0
}

func lookupHeight(project session.Project, id string) int64 {
	if idx := findScreen(project.Screens, id); idx >= 0 {
		return project.Screens[idx].Height
	}
	return 0
}
