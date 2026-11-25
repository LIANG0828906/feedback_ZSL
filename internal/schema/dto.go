package schema

import "combatjudge/internal/model"

type envelope struct {
	Schema       string `json:"schema"`
	SchemaCamel  string `json:"schemaVersion"`
	SchemaPascal string `json:"Schema"`
}

type Vec3DTO [3]int64

type FileV1 struct {
	Schema      string       `json:"Schema"`
	BattleID    string       `json:"BattleId"`
	FrameStepMS *int64       `json:"FrameStepMs"`
	UnitScale   int64        `json:"UnitScale,omitempty"`
	Colliders   []ColliderV1 `json:"Colliders"`
	Sweeps      []SweepV1    `json:"Sweeps"`
	Requests    []RequestV1  `json:"Requests"`
}

type ColliderV1 struct {
	No             int      `json:"No"`
	ID             string   `json:"Id"`
	Kind           string   `json:"Kind"`
	Part           string   `json:"Part"`
	Min            *Vec3DTO `json:"Min"`
	Max            *Vec3DTO `json:"Max"`
	CapStart       *Vec3DTO `json:"CapStart"`
	CapEnd         *Vec3DTO `json:"CapEnd"`
	RadiusPermille *int64   `json:"RadiusPermille"`
}

type SweepV1 struct {
	No             int     `json:"No"`
	ID             string  `json:"Id"`
	StartPosQ      Vec3DTO `json:"StartPosQ"`
	EndPosQ        Vec3DTO `json:"EndPosQ"`
	RadiusPermille *int64  `json:"RadiusPermille"`
	StartFrame     *int    `json:"StartFrame"`
	EndFrame       *int    `json:"EndFrame"`
	DamagePermille *int64  `json:"DamagePermille"`
}

type RequestV1 struct {
	Frame   *int   `json:"Frame"`
	SweepID string `json:"SweepId"`
}

type FileV2 struct {
	Schema      string       `json:"schema"`
	BattleID    string       `json:"battle_id"`
	FrameStepMS *int64       `json:"frame_step_ms"`
	UnitScale   int64        `json:"unit_scale,omitempty"`
	Colliders   []ColliderV2 `json:"colliders"`
	Sweeps      []SweepV2    `json:"sweeps"`
	Requests    []RequestV2  `json:"requests"`
}

type ColliderV2 struct {
	No             int      `json:"collider_no"`
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Part           string   `json:"part"`
	MinsQ          *Vec3DTO `json:"mins_q"`
	MaxsQ          *Vec3DTO `json:"maxs_q"`
	CenterQ        *Vec3DTO `json:"center_q"`
	HalfAxisDeltaQ *Vec3DTO `json:"half_axis_delta_q"`
	RadiusMyriad   *int64   `json:"radius_myriad"`
}

type SweepV2 struct {
	No            int     `json:"sweep_no"`
	ID            string  `json:"sweep_id"`
	OriginQ       Vec3DTO `json:"origin_q"`
	AdvanceDeltaQ Vec3DTO `json:"advance_delta_q"`
	RadiusMyriad  *int64  `json:"radius_myriad"`
	StartMS       *int64  `json:"start_ms"`
	EndMS         *int64  `json:"end_ms"`
	DamageMyriad  *int64  `json:"damage_myriad"`
	Penetrate     *bool   `json:"penetrate"`
	DecayMyriad   *int64  `json:"decay_myriad"`
}

type RequestV2 struct {
	AtMS    *int64 `json:"at_ms"`
	SweepID string `json:"sweep_id"`
}

type FileV3 struct {
	SchemaVersion string       `json:"schemaVersion"`
	BattleID      string       `json:"battleId"`
	FrameStepMS   *int64       `json:"frameStepMs"`
	Colliders     []ColliderV3 `json:"colliders"`
	Sweeps        []SweepV3    `json:"sweeps"`
	Requests      []RequestV3  `json:"requests"`
}

type ColliderV3 struct {
	No     int         `json:"colliderNo"`
	ID     string      `json:"id"`
	Kind   string      `json:"kind"`
	Part   string      `json:"part"`
	Min    *model.Vec3 `json:"min,omitempty"`
	Max    *model.Vec3 `json:"max,omitempty"`
	P0     *model.Vec3 `json:"p0,omitempty"`
	P1     *model.Vec3 `json:"p1,omitempty"`
	Radius int64       `json:"radiusQ"`
}

type SweepV3 struct {
	No           int        `json:"sweepNo"`
	ID           string     `json:"id"`
	From         model.Vec3 `json:"fromQ"`
	To           model.Vec3 `json:"toQ"`
	Radius       int64      `json:"radiusQ"`
	StartFrame   int        `json:"startFrame"`
	EndFrame     int        `json:"endFrame"`
	Damage       int64      `json:"damageQ"`
	Penetration  *bool      `json:"penetration,omitempty"`
	Decay        *int64     `json:"decayQ,omitempty"`
	MultiHit     *bool      `json:"multiHit,omitempty"`
	MaxSegments  *int       `json:"maxSegments,omitempty"`
	RepeatPolicy string     `json:"repeatPolicy,omitempty"`
	EndpointMode string     `json:"endpointMode,omitempty"`
}

type RequestV3 struct {
	Frame   int    `json:"frame"`
	SweepID string `json:"sweepId"`
}

type LoadResult struct {
	Project *model.Project
}
