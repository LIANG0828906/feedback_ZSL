package generator

import (
	"fmt"
	"math"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/engine"
	"mirrorcast/internal/session"
)

const DefaultSeed int64 = 0x42A8691E

type Suite struct {
	Seed     int64              `json:"seed"`
	Projects []GeneratedProject `json:"projects"`
	Critical CriticalFile       `json:"critical_cases"`
}

type GeneratedProject struct {
	Name    string            `json:"name"`
	Project session.Project   `json:"project"`
	Truth   session.TruthFile `json:"truth"`
}

type CriticalFile struct {
	Seed  int64          `json:"seed"`
	Cases []CriticalCase `json:"cases"`
}

type CriticalInput struct {
	Master       *session.Rect `json:"master,omitempty"`
	Width        int64         `json:"width,omitempty"`
	Height       int64         `json:"height,omitempty"`
	Scale        int64         `json:"scale_permyriad,omitempty"`
	Chroma       string        `json:"chroma,omitempty"`
	Budget       int64         `json:"budget_kbps,omitempty"`
	Threads      int           `json:"threads,omitempty"`
	BufferBudget int64         `json:"buffer_budget_units,omitempty"`
}

type CriticalCase struct {
	ID               string           `json:"id"`
	Kind             string           `json:"kind"`
	Input            CriticalInput    `json:"input"`
	ExpectedCode     string           `json:"expected_code,omitempty"`
	Expected         *session.Mapping `json:"expected_mapping,omitempty"`
	ExpectedDecision session.Decision `json:"expected_decision,omitempty"`
}

type rng struct{ state uint64 }

func newRNG(seed int64) *rng { return &rng{state: uint64(seed)} }

func (r *rng) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *rng) pickInt64(values []int64) int64 { return values[r.intn(len(values))] }

func GenerateSuite(seed int64) (*Suite, error) {
	suite := &Suite{Seed: seed}

	canonical, err := canonicalProject(seed)
	if err != nil {
		return nil, err
	}
	randoms, err := randomProjects(seed)
	if err != nil {
		return nil, err
	}
	all := append([]GeneratedProject{canonical}, randoms...)
	for i := range all {
		truth, err := BuildTruth(all[i].Project)
		if err != nil {
			return nil, fmt.Errorf("truth for %s: %w", all[i].Name, err)
		}
		all[i].Truth = truth
	}
	suite.Projects = all
	suite.Critical = criticalCases(seed)
	return suite, nil
}

func BuildTruth(project session.Project) (session.TruthFile, error) {
	eng, err := engine.New(project)
	if err != nil {
		return session.TruthFile{}, err
	}
	ticks, err := eng.RunAll()
	if err != nil {
		return session.TruthFile{}, err
	}
	return session.TruthFile{
		ProjectID:  project.ProjectID,
		Seed:       project.Seed,
		Tolerances: session.DeclaredTolerances(),
		Ticks:      ticks,
	}, nil
}

func canonicalProject(seed int64) (GeneratedProject, error) {
	master := session.Rect{X: 16, Y: 9, Width: 1920, Height: 1080}
	screens := []session.Screen{
		{ID: "s01", Host: "h1", Width: 320, Height: 200, ScalePermyriad: 10000, MinScalePermyriad: 5000, Chroma: session.Chroma420, Priority: 10},
		{ID: "s02", Host: "h1", Width: 320, Height: 200, ScalePermyriad: 7500, MinScalePermyriad: 5000, Chroma: session.Chroma422, Priority: 5},
		{ID: "s03", Host: "h2", Width: 320, Height: 200, ScalePermyriad: 10000, MinScalePermyriad: 2500, Chroma: session.Chroma420, Priority: 1},
		{ID: "s04", Host: "h2", Width: 320, Height: 200, ScalePermyriad: 10000, MinScalePermyriad: 5000, Chroma: session.Chroma444, Priority: 4},
	}
	topology2 := &session.Topology{Version: 2, Hosts: []session.TopologyHost{
		{ID: "h1", ScreenIDs: []string{"s02", "s01", "s04"}},
		{ID: "h2", ScreenIDs: []string{"s03"}},
	}}
	events := []session.Event{
		{ID: "e01", Tick: 1, Type: session.EventJoin, ScreenID: "s01"},
		{ID: "e02", Tick: 2, Type: session.EventJoin, ScreenID: "s02"},
		{ID: "e03", Tick: 3, Type: session.EventJoin, ScreenID: "s03"},
		{ID: "e04", Tick: 4, Type: session.EventJoin, ScreenID: "s04"},
		{ID: "e05", Tick: 5, Type: session.EventTopology, Topology: topology2},
		{ID: "e06", Tick: 6, Type: session.EventLeave, ScreenID: "s02"},
		{ID: "e07", Tick: 7, Type: session.EventJoin, ScreenID: "s04"},
		{ID: "e08", Tick: 8, Type: session.EventBudget, Budget: 45000},
	}
	project := session.Project{
		ProjectID: "canonical-budget-topology", Seed: seed, Master: master,
		TotalBudget: 80000, Threads: 4, BufferBudget: 1 << 20,
		Screens: screens, Events: events,
	}
	return GeneratedProject{Name: "01-canonical", Project: project}, nil
}

type resolutionChoice struct{ w, h int64 }

func randomProjects(seed int64) ([]GeneratedProject, error) {
	r := newRNG(seed ^ 0x51A7E)
	resolutions := []resolutionChoice{
		{320, 200}, {400, 300}, {640, 400}, {800, 600},
		{1000, 800}, {1200, 800}, {1600, 1000}, {2000, 1000},
	}
	chromas := []session.ChromaFormat{session.Chroma420, session.Chroma422, session.Chroma444}
	out := make([]GeneratedProject, 0, 4)
	for p := 0; p < 4; p++ {
		count := 5 + r.intn(4)
		screens := make([]session.Screen, 0, count)
		costs := make([]int64, 0, count)
		var requestedTotal int64
		for i := 0; i < count; i++ {
			res := resolutions[r.intn(len(resolutions))]
			requestedScale := r.pickInt64([]int64{10000, 8750, 7500, 6250, 5000})
			minScale := r.pickInt64([]int64{3750, 2500})
			if minScale > requestedScale {
				minScale = 2500
			}
			chroma := chromas[r.intn(len(chromas))]
			id := fmt.Sprintf("r%ds%02d", p+1, i+1)
			screen := session.Screen{
				ID: id, Host: fmt.Sprintf("h%d", 1+r.intn(3)),
				Width: res.w, Height: res.h,
				ScalePermyriad: requestedScale, MinScalePermyriad: minScale,
				Chroma: chroma, Priority: int64(1 + r.intn(9)),
			}
			mapping, err := session.MapRegion(session.Rect{X: 0, Y: 0, Width: 1600, Height: 900}, screen.Width, screen.Height, screen.ScalePermyriad, string(screen.Chroma))
			if err != nil {
				return nil, err
			}
			requestedTotal += mapping.BudgetCostKbps
			costs = append(costs, mapping.BudgetCostKbps)
			screens = append(screens, screen)
		}

		budgetFactor := []int64{45, 60, 75, 95}[r.intn(4)]
		initialBudget := requestedTotal * budgetFactor / 100
		if initialBudget < costs[0] {
			initialBudget = costs[0]
		}
		events := make([]session.Event, 0, count+4)
		tick := int64(1)
		for i := range screens {
			events = append(events, session.Event{
				ID: fmt.Sprintf("r%d-e%02d-join", p+1, tick), Tick: tick,
				Type: session.EventJoin, ScreenID: screens[i].ID,
			})
			tick++
		}
		leaveIndex := 0
		events = append(events, session.Event{
			ID: fmt.Sprintf("r%d-e%02d-leave", p+1, tick), Tick: tick,
			Type: session.EventLeave, ScreenID: screens[leaveIndex].ID,
		})
		tick++
		topology := randomTopology(screens, int64(2))
		events = append(events, session.Event{
			ID: fmt.Sprintf("r%d-e%02d-topology", p+1, tick), Tick: tick,
			Type: session.EventTopology, Topology: &topology,
		})
		tick++
		budgetEvent := initialBudget * []int64{55, 70, 110}[r.intn(3)] / 100
		events = append(events, session.Event{
			ID: fmt.Sprintf("r%d-e%02d-budget", p+1, tick), Tick: tick,
			Type: session.EventBudget, Budget: budgetEvent,
		})

		project := session.Project{
			ProjectID:    fmt.Sprintf("random-%02d-seed-%x", p+1, seed),
			Seed:         seed + int64(p+1)*7919,
			Master:       session.Rect{X: 8, Y: 4, Width: 1600, Height: 900},
			TotalBudget:  initialBudget,
			Threads:      2 + r.intn(16),
			BufferBudget: 1 << (16 + r.intn(12)),
			Screens:      screens,
			Events:       events,
		}
		all, err := sessionInitialTopologyVersion(project)
		if err != nil {
			return nil, err
		}
		_ = all
		out = append(out, GeneratedProject{Name: fmt.Sprintf("%02d-random-%d", p+2, p+1), Project: project})
	}
	return out, nil
}

func sessionInitialTopologyVersion(project session.Project) (bool, error) {
	// Engine.New performs full validation; callers later build truth. This
	// early check keeps generator failures deterministic and local.
	if err := session.ValidateProject(&project); err != nil {
		return false, err
	}
	return true, nil
}

func randomTopology(screens []session.Screen, version int64) session.Topology {
	hostMap := map[string][]string{}
	hostOrder := []string{}
	for _, screen := range screens {
		if _, ok := hostMap[screen.Host]; !ok {
			hostOrder = append(hostOrder, screen.Host)
		}
		hostMap[screen.Host] = append(hostMap[screen.Host], screen.ID)
	}
	hosts := make([]session.TopologyHost, 0, len(hostOrder))
	for hi, host := range hostOrder {
		ids := append([]string(nil), hostMap[host]...)
		if hi%2 == 0 {
			for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
		hosts = append(hosts, session.TopologyHost{ID: host, ScreenIDs: ids})
	}
	return session.Topology{Version: version, Hosts: hosts}
}

func criticalCases(seed int64) CriticalFile {
	master1 := session.Rect{X: 0, Y: 0, Width: 1, Height: 1}
	master21 := session.Rect{X: 0, Y: 0, Width: 2, Height: 1}
	exact420, _ := session.MapRegion(master1, 100, 100, 200, "420")
	exact422, _ := session.MapRegion(master21, 100, 50, 200, "422")
	exact444, _ := session.MapRegion(master1, 100, 100, 100, "444")
	return CriticalFile{Seed: seed, Cases: []CriticalCase{
		{ID: "crit-420-exact-unit", Kind: "mapping", Input: CriticalInput{Master: &master1, Width: 100, Height: 100, Scale: 200, Chroma: "420", Budget: exact420.BudgetCostKbps}, Expected: &exact420, ExpectedDecision: session.DecisionAdmitted},
		{ID: "crit-422-exact-unit", Kind: "mapping", Input: CriticalInput{Master: &master21, Width: 100, Height: 50, Scale: 200, Chroma: "422", Budget: exact422.BudgetCostKbps}, Expected: &exact422, ExpectedDecision: session.DecisionAdmitted},
		{ID: "crit-444-exact-unit", Kind: "mapping", Input: CriticalInput{Master: &master1, Width: 100, Height: 100, Scale: 100, Chroma: "444", Budget: exact444.BudgetCostKbps}, Expected: &exact444, ExpectedDecision: session.DecisionAdmitted},
		{ID: "crit-420-one-pixel-short", Kind: "mapping_error", Input: CriticalInput{Master: &master1, Width: 100, Height: 100, Scale: 100, Chroma: "420"}, ExpectedCode: bizerror.EResolutionTooSmall},
		{ID: "crit-scale-zero", Kind: "mapping_error", Input: CriticalInput{Master: &master1, Width: 320, Height: 200, Scale: 0, Chroma: "420"}, ExpectedCode: bizerror.EScaleZero},
		{ID: "crit-scale-negative", Kind: "mapping_error", Input: CriticalInput{Master: &master1, Width: 320, Height: 200, Scale: -1, Chroma: "420"}, ExpectedCode: bizerror.EScaleOutOfRange},
		{ID: "crit-scale-too-large", Kind: "mapping_error", Input: CriticalInput{Master: &master1, Width: 320, Height: 200, Scale: 10001, Chroma: "420"}, ExpectedCode: bizerror.EScaleOutOfRange},
		{ID: "crit-resolution-negative", Kind: "mapping_error", Input: CriticalInput{Master: &master1, Width: -320, Height: 200, Scale: 10000, Chroma: "420"}, ExpectedCode: bizerror.EResolutionNegative},
		{ID: "crit-chroma-invalid", Kind: "mapping_error", Input: CriticalInput{Master: &master1, Width: 320, Height: 200, Scale: 10000, Chroma: "PLANAR_411"}, ExpectedCode: bizerror.EChromaEnumInvalid},
		{ID: "crit-arithmetic-overflow", Kind: "mapping_error", Input: CriticalInput{Master: &session.Rect{Width: 1, Height: 1}, Width: math.MaxInt64, Height: math.MaxInt64, Scale: 10000, Chroma: "444"}, ExpectedCode: bizerror.EOverflow},
		{ID: "crit-threads-out-of-range", Kind: "project_error", Input: CriticalInput{Threads: 128}, ExpectedCode: bizerror.EThreadOutOfRange},
		{ID: "crit-buffer-out-of-range", Kind: "project_error", Input: CriticalInput{BufferBudget: -1}, ExpectedCode: bizerror.EBufferBudgetOutOfRange},
		{ID: "crit-budget-exact-fit", Kind: "budget_boundary", Input: CriticalInput{Master: &master1, Width: 100, Height: 100, Scale: 200, Chroma: "420", Budget: exact420.BudgetCostKbps}, Expected: &exact420, ExpectedDecision: session.DecisionAdmitted},
		{ID: "crit-budget-one-kbps-short", Kind: "budget_boundary", Input: CriticalInput{Master: &master1, Width: 100, Height: 100, Scale: 200, Chroma: "420", Budget: exact420.BudgetCostKbps - 1}, Expected: &exact420, ExpectedDecision: session.DecisionRejected},
	},
	}
}
