package sample

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"storyboardstudio/internal/model"
)

func DefaultSeeds() []int64 {
	return []int64{252001, 252002, 252003}
}

func GenerateAll(directory string, seeds ...int64) ([]string, error) {
	if len(seeds) == 0 {
		seeds = DefaultSeeds()
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	written := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		project := Generate(seed)
		data, err := json.MarshalIndent(project, "", "  ")
		if err != nil {
			return nil, err
		}
		data = append(data, '\n')
		path := filepath.Join(directory, fmt.Sprintf("project-%d.json", seed))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
}

func Generate(seed int64) *model.Project {
	rng := rand.New(rand.NewSource(seed))
	branchClue := seed%2 == 0
	endingArrest := (seed/2)%2 == 0

	durations := []int64{500, 750, 1000, 1250, 1500, 2000}
	aspects := []string{"aspect://16x9", "aspect://21x9", "aspect://4x3"}
	pickDuration := func() int64 { return durations[rng.Intn(len(durations))] }
	pickAspect := func() string { return aspects[rng.Intn(len(aspects))] }

	n00Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-00-a", seed), pickDuration(), "aspect://21x9", "slow aerial establish", "drift", "fade"),
	}
	n01Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-01-a", seed), pickDuration(), pickAspect(), "medium desk", "static", "cut"),
		makeShot(fmt.Sprintf("s%d-01-b", seed), pickDuration(), "aspect://16x9", "close choice", "push-in", "cut"),
	}
	n02Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-02-a", seed), pickDuration(), "aspect://16x9", "clue insert", "macro-pan", "match-cut"),
	}
	n03Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-03-a", seed), pickDuration(), "aspect://21x9", "street exit", "tracking", "wipe"),
		makeShot(fmt.Sprintf("s%d-03-b", seed), pickDuration(), pickAspect(), "witness glance", "handheld", "cut"),
	}
	n04Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-04-a", seed), pickDuration(), "aspect://16x9", "converging reaction", "static", "cross-dissolve"),
	}
	n05Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-05-a", seed), pickDuration(), pickAspect(), "decision close-up", "push-in", "cut"),
	}
	n06Shots := []model.Shot{
		makeShot(fmt.Sprintf("s%d-06-a", seed), pickDuration(), "aspect://21x9", "epilogue skyline", "pull-back", "fade-out"),
	}

	branchOption := "inspect_clue"
	branchFlag := "path_clue"
	branchNode := "n02_clue"
	if !branchClue {
		branchOption = "walk_away"
		branchFlag = "path_leave"
		branchNode = "n03_leave"
	}
	endingOption := "arrest"
	endingFlag := "ending_arrest"
	if !endingArrest {
		endingOption = "surveil"
		endingFlag = "ending_watch"
	}

	observable := []string{
		"world_established", "office_entered", "path_clue", "path_leave", "has_clue",
		"clue_found", "witness_met", "left_office", "branch_resolved", "ending_arrest",
		"ending_watch", "case_closed",
	}

	project := &model.Project{
		Schema:           model.ProjectSchemaV1,
		ID:               fmt.Sprintf("narrative-seed-%d", seed),
		Seed:             seed,
		SampleIntervalMS: 250,
		ToleranceMS:      25,
		ObservableFlags:  observable,
		Nodes: []model.Node{
			{
				ID: "n00_world", Title: "城市开场", Shots: n00Shots,
				OnArrive: model.Effects{SetFlags: []string{"world_established"}},
				Layout:   &model.Layout{X: 40, Y: 180},
			},
			{
				ID: "n01_office", Title: "事务所选择", Parents: []string{}, Shots: n01Shots,
				OnArrive: model.Effects{SetFlags: []string{"office_entered"}},
				Options: []model.ChoiceOption{
					{ID: "inspect_clue", Label: "检查线索", Effects: model.Effects{SetFlags: []string{"path_clue"}}},
					{ID: "walk_away", Label: "离开事务所", Effects: model.Effects{SetFlags: []string{"path_leave"}}},
				},
				Layout: &model.Layout{X: 260, Y: 180},
			},
			{
				ID: "n02_clue", Title: "发现线索", Parents: []string{"n01_office"}, Shots: n02Shots,
				Preconditions: model.Condition{All: []string{"path_clue"}, None: []string{"path_leave"}},
				MutexWith:     []string{"n03_leave"},
				OnArrive: model.Effects{
					SetFlags: []string{"has_clue", "clue_found"},
					Characters: []model.CharacterChange{
						{CharacterID: "detective", Key: "mood", Value: "focused"},
					},
				},
				Layout: &model.Layout{X: 500, Y: 70},
			},
			{
				ID: "n03_leave", Title: "街头目击者", Parents: []string{"n01_office"}, Shots: n03Shots,
				Preconditions: model.Condition{All: []string{"path_leave"}, None: []string{"path_clue"}},
				MutexWith:     []string{"n02_clue"},
				OnArrive: model.Effects{
					SetFlags: []string{"witness_met", "left_office"},
					Characters: []model.CharacterChange{
						{CharacterID: "detective", Key: "mood", Value: "restless"},
					},
				},
				Layout: &model.Layout{X: 500, Y: 290},
			},
			{
				ID: "n04_join", Title: "因果汇聚", Parents: []string{"n02_clue", "n03_leave"},
				Convergence: &model.Convergence{ID: "join_branch", Policy: "any", Predecessors: []string{"n02_clue", "n03_leave"}},
				Shots:       n04Shots,
				OnArrive:    model.Effects{SetFlags: []string{"branch_resolved"}},
				Layout:      &model.Layout{X: 740, Y: 180},
			},
			{
				ID: "n05_final_choice", Title: "最终行动", Parents: []string{"n04_join"}, Shots: n05Shots,
				Options: []model.ChoiceOption{
					{ID: "arrest", Label: "立即逮捕", Effects: model.Effects{SetFlags: []string{"ending_arrest"}}},
					{ID: "surveil", Label: "继续监视", Effects: model.Effects{SetFlags: []string{"ending_watch"}}},
				},
				Layout: &model.Layout{X: 980, Y: 180},
			},
			{
				ID: "n06_epilogue", Title: "结案余韵", Parents: []string{"n05_final_choice"}, Shots: n06Shots,
				OnArrive: model.Effects{
					SetFlags:   []string{"case_closed"},
					ClearFlags: []string{"office_entered"},
					Characters: []model.CharacterChange{
						{CharacterID: "detective", Key: "case_status", Value: "closed"},
					},
				},
				Layout: &model.Layout{X: 1220, Y: 180},
			},
		},
		ChoiceSequence: model.ChoiceSequence{
			ID: fmt.Sprintf("choices-%d", seed),
			Items: []model.Choice{
				{Index: 0, NodeID: "n01_office", OptionID: branchOption},
				{Index: 1, NodeID: "n05_final_choice", OptionID: endingOption},
			},
		},
	}

	eventOrder := []string{"n00_world", "n01_office", branchNode, "n04_join", "n05_final_choice", "n06_epilogue"}
	project.Truth = buildTruth(project, eventOrder, branchFlag, endingFlag)
	return project
}

func makeShot(id string, durationMS int64, aspect, camera, motion, transition string) model.Shot {
	return model.Shot{
		ID: id, DurationMS: durationMS, DurationPresent: true, AspectPath: aspect,
		Camera: camera, Motion: motion, Transition: transition,
	}
}

func buildTruth(project *model.Project, eventOrder []string, branchFlag, endingFlag string) model.Truth {
	byID := map[string]*model.Node{}
	for i := range project.Nodes {
		byID[project.Nodes[i].ID] = &project.Nodes[i]
	}
	terminal := map[string]bool{}
	for _, flag := range project.ObservableFlags {
		terminal[flag] = false
	}
	terminal["world_established"] = true
	terminal[branchFlag] = true
	terminal["branch_resolved"] = true
	terminal[endingFlag] = true
	terminal["case_closed"] = true

	switch branchFlag {
	case "path_clue":
		terminal["has_clue"] = true
		terminal["clue_found"] = true
	case "path_leave":
		terminal["witness_met"] = true
		terminal["left_office"] = true
	}

	truthShots := []model.ShotTruth{}
	globalIndex := 0
	for _, nodeID := range eventOrder {
		node := byID[nodeID]
		for _, shot := range node.Shots {
			truthShots = append(truthShots, model.ShotTruth{
				GlobalIndex: globalIndex,
				NodeID:      nodeID,
				ShotID:      shot.ID,
				SamplesMS:   expectedSamples(shot.DurationMS, project.SampleIntervalMS),
			})
			globalIndex++
		}
	}

	return model.Truth{
		TerminalFlags:   terminal,
		EventOrder:      append([]string(nil), eventOrder...),
		ReachableNodes:  append([]string(nil), eventOrder...),
		KeyReachability: map[string]bool{"n02_clue": branchFlag == "path_clue", "n03_leave": branchFlag == "path_leave", "n04_join": true},
		ShotSamples:     truthShots,
	}
}

func expectedSamples(duration, interval int64) []int64 {
	if duration == 0 {
		return []int64{0}
	}
	result := []int64{}
	for at := int64(0); at < duration; at += interval {
		result = append(result, at)
	}
	result = append(result, duration)
	return result
}
