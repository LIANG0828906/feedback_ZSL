package sample

import (
	"encoding/json"
	"fmt"
	"math/rand"

	"battlekernel/internal/loader"
	"battlekernel/internal/model"
)

const DefaultSeed int64 = 20261125

type Sample struct {
	Index             int            `json:"index"`
	Project           *model.Project `json:"project"`
	Documents         map[int][]byte `json:"-"`
	TruthJSON         []byte         `json:"-"`
	FingerprintJSON   []byte         `json:"-"`
	RoundFingerprints []string       `json:"round_fingerprints"`
	Fingerprint       string         `json:"fingerprint"`
}

type Suite struct {
	Seed    int64     `json:"seed"`
	Samples []*Sample `json:"samples"`
}

type FingerprintFile struct {
	Seed      int64                `json:"seed"`
	Algorithm string               `json:"algorithm_version"`
	Projects  []ProjectFingerprint `json:"projects"`
}

type ProjectFingerprint struct {
	ProjectID string   `json:"project_id"`
	Truth     string   `json:"truth_fingerprint"`
	Rounds    []string `json:"round_fingerprints"`
}

func GenerateSuite(seed int64, count int) (*Suite, error) {
	if count < 1 {
		count = 1
	}
	suite := &Suite{Seed: seed}
	for i := 0; i < count; i++ {
		s, err := generateBattle(seed, i)
		if err != nil {
			return nil, err
		}
		suite.Samples = append(suite.Samples, s)
	}
	return suite, nil
}

func generateBattle(baseSeed int64, index int) (*Sample, error) {
	rng := rand.New(rand.NewSource(baseSeed + int64(index*9973)))
	roundCount := 6
	startRound := int64(1)
	tickMs := int64(1000)
	epochMs := int64(1760000000000)
	id := fmt.Sprintf("battle-%03d", index+1)

	resourceU2 := []int{1, 0, 2}[index%3]
	enemyHP := 68 + rng.Intn(9)

	p := &model.Project{
		ID: id, Seed: baseSeed + int64(index*9973), FormatNo: 0, SourceFormat: "generator-canonical",
		SchemaVersion: model.CanonicalFormatVersion, TickMs: tickMs, EpochMs: epochMs,
		StartRound: startRound, TotalRounds: roundCount, EnemyHP: enemyHP,
		Units: []model.Unit{
			{ID: "u1", Name: "Vanguard", BaseWeight: 8, TieOrdinal: 1, InitialCooldown: 0, MaxResource: 12, InitialResource: 6 + index%2, Regen: 2},
			{ID: "u2", Name: "Skirmisher", BaseWeight: 5, TieOrdinal: 2, InitialCooldown: 0, MaxResource: 10, InitialResource: resourceU2, Regen: 2},
			{ID: "u3", Name: "Oracle", BaseWeight: 5, TieOrdinal: 3, InitialCooldown: 1, MaxResource: 12, InitialResource: 4, Regen: 2},
			{ID: "u4", Name: "Darter", BaseWeight: 3, TieOrdinal: 4, InitialCooldown: 0, MaxResource: 8, InitialResource: 8, Regen: 1},
		},
		Skills: []model.Skill{
			{ID: "s1-heavy", UnitID: "u1", Cost: 3, Cooldown: 1, Power: 6},
			{ID: "s2-quick", UnitID: "u2", Cost: 2, Cooldown: 0, Power: 4},
			{ID: "s3-rite", UnitID: "u3", Cost: 3, Cooldown: 0, Power: 8, Conditional: true},
			{ID: "s4-jab", UnitID: "u4", Cost: 1, Cooldown: 0, Power: 3},
		},
		Reorders: []model.ReorderInstruction{
			{Offset: 0, ID: "ro-u3-after-r2", UnitID: "u3", AfterRound: 2, NewTieOrdinal: 0, Reason: "formation moves Oracle ahead of equal-weight Skirmisher after round 2"},
		},
	}
	for _, u := range p.Units {
		p.InitialStates = append(p.InitialStates, model.UnitState{UnitID: u.ID, Resource: u.InitialResource, CooldownRemaining: u.InitialCooldown, Alive: true})
	}

	flagPattern := [][]bool{
		{false, false, true, true, true, true},
		{false, true, true, false, true, true},
		{true, false, false, true, true, true},
	}[index%3]

	for r := 0; r < roundCount; r++ {
		roundNo := startRound + int64(r)
		ts := epochMs + (roundNo-startRound)*tickMs
		spec := model.RoundSpec{Round: roundNo, TimestampMs: ts}
		for ui, u := range p.Units {
			skill := p.Skills[ui]
			action := model.PlannedAction{UnitID: u.ID, SkillID: skill.ID}
			if skill.Conditional {
				flag := flagPattern[r]
				action.Flag = &flag
			}
			spec.Actions = append(spec.Actions, action)

			p.SkillSequence = append(p.SkillSequence, model.SequenceItem{
				Round: roundNo, UnitID: u.ID, SkillID: skill.ID,
				Ref: fmt.Sprintf("S|%d|%s|%s", roundNo, u.ID, skill.ID),
			})
			p.ActorSequence = append(p.ActorSequence, model.SequenceItem{
				Round: roundNo, UnitID: u.ID,
				Ref: fmt.Sprintf("A|%d|%s", roundNo, u.ID),
			})
		}
		p.Rounds = append(p.Rounds, spec)
	}

	truth, err := BuildReferenceTruth(p)
	if err != nil {
		return nil, err
	}
	p.Truth = truth
	roundFPS, fp, err := loader.TruthFingerprint(truth)
	if err != nil {
		return nil, err
	}
	truth.RoundHashes = roundFPS
	truth.Fingerprint = fp

	docs := map[int][]byte{}
	for _, formatNo := range []int{1, 2, 3} {
		data, err := loader.Encode(p, formatNo)
		if err != nil {
			return nil, err
		}
		docs[formatNo] = data
	}
	truthJSON, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	fpFile := FingerprintFile{Seed: baseSeed, Algorithm: model.AlgorithmVersion}
	fpFile.Projects = append(fpFile.Projects, ProjectFingerprint{ProjectID: p.ID, Truth: fp, Rounds: roundFPS})
	fpJSON, err := json.MarshalIndent(fpFile, "", "  ")
	if err != nil {
		return nil, err
	}

	return &Sample{Project: p, Documents: docs, TruthJSON: truthJSON, FingerprintJSON: fpJSON, RoundFingerprints: roundFPS, Fingerprint: fp}, nil
}

func (s *Suite) FingerprintFile() FingerprintFile {
	file := FingerprintFile{Seed: s.Seed, Algorithm: model.AlgorithmVersion}
	for _, sample := range s.Samples {
		file.Projects = append(file.Projects, ProjectFingerprint{ProjectID: sample.Project.ID, Truth: sample.Fingerprint, Rounds: sample.RoundFingerprints})
	}
	return file
}
