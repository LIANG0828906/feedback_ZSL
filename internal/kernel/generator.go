package kernel

import (
	"fmt"
	"math/rand"
)

const DefaultSeed int64 = 0x42A8691E

type SampleSet struct {
	Seed      int64             `json:"seed"`
	Scene     *Scene            `json:"scene"`
	Report    *PredictionReport `json:"report"`
	Truth     *Truth            `json:"truth"`
	Camera    Camera            `json:"camera"`
	Documents map[int][]byte    `json:"documents"`
	TruthJSON []byte            `json:"-"`
}

func GenerateSampleSet(seed int64) (*SampleSet, error) {
	rng := rand.New(rand.NewSource(seed))
	jitterY := 252 + rng.Int63n(17) - 8
	levelID := fmt.Sprintf("deterministic-bounce-%08x", uint64(seed)&0xffffffff)

	world := func(x, y int64) Vec2 { return Vec2{X: x * PosScale, Y: y * PosScale} }
	segments := []Segment{
		{
			ID: "floor_main", MaterialID: "stone", MaterialVersion: 1,
			A: world(620, 620), B: world(180, 620), NormalSide: "left",
			Restitution: 8200,
		},
		{
			ID: "ramp_right", MaterialID: "metal", MaterialVersion: 1,
			A: world(1050, 430), B: world(700, 650), NormalSide: "left",
			Restitution: 7600,
		},
		{
			ID: "wall_right", MaterialID: "brick", MaterialVersion: 1,
			A: world(940, 360), B: world(940, 600), NormalSide: "left",
			Restitution: 8800,
		},
		{
			ID: "platform_small", MaterialID: "wood", MaterialVersion: 1,
			A: world(760, 548), B: world(620, 548), NormalSide: "left",
			Restitution: 6800,
		},
		{
			ID: "remote_plate", MaterialID: "glass", MaterialVersion: 1,
			A: world(42, jitterY), B: world(132, jitterY), NormalSide: "left",
			Restitution: 5000,
		},
	}

	launchers := []Launcher{
		{ID: "gun_vertical", Position: world(360, 120), Velocity: Vec2{X: 45 * PosScale, Y: 0}},
		{ID: "gun_diagonal", Position: world(120, 140), Velocity: Vec2{X: 330 * PosScale, Y: -160 * PosScale}},
		{ID: "gun_wall", Position: world(140, 300), Velocity: Vec2{X: 850 * PosScale, Y: -180 * PosScale}},
		{ID: "gun_escape", Position: world(40, 120), Velocity: Vec2{X: 1050 * PosScale, Y: -100 * PosScale}},
	}
	requests := []PredictionRequest{
		{ID: "req_vertical", LogicalFrame: 0, LauncherID: "gun_vertical", MaxFrames: 72},
		{ID: "req_diagonal", LogicalFrame: 1, LauncherID: "gun_diagonal", MaxFrames: 72},
		{ID: "req_wall", LogicalFrame: 2, LauncherID: "gun_wall", MaxFrames: 40},
		{ID: "req_escape", LogicalFrame: 3, LauncherID: "gun_escape", MaxFrames: 30},
	}

	scene := &Scene{
		SchemaVersion:    SchemaV3,
		LevelID:          levelID,
		Seed:             seed,
		Width:            DefaultWidthUnits,
		Height:           DefaultHeightUnits,
		MSPerTick:        DefaultTickMS,
		StepTicks:        1,
		StepMS:           DefaultTickMS,
		Gravity:          Vec2{X: 0, Y: 700 * PosScale},
		Segments:         segments,
		Launchers:        launchers,
		Requests:         requests,
		Versions:         SemanticVersions{Endpoints: 1, Normals: 1, Restitutions: 1, Gravity: 1, TimeStep: 1, Materials: 1},
		StructureVersion: 1,
	}

	report, err := Predict(scene)
	if err != nil {
		return nil, err
	}
	truth := BuildTruth(scene, report)
	truthJSON, err := MarshalTruth(truth)
	if err != nil {
		return nil, err
	}
	docs := map[int][]byte{}
	for _, version := range []int{SchemaV1, SchemaV2, SchemaV3} {
		exported, err := Export(scene, version)
		if err != nil {
			return nil, err
		}
		docs[version] = exported.Data
	}

	return &SampleSet{
		Seed:      seed,
		Scene:     scene,
		Report:    report,
		Truth:     truth,
		Camera:    DefaultCamera(),
		Documents: docs,
		TruthJSON: truthJSON,
	}, nil
}
