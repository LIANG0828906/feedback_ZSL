package solver

import (
	"encoding/json"

	"retargetlab/internal/geom"
	"retargetlab/internal/model"
)

const DefaultAlgorithmVersion = "retarget-solver/v1"

type Params struct {
	ScaleOverrides   map[string]float64 `json:"scaleOverrides,omitempty"`
	ParamVersion     int                `json:"paramVersion"`
	AlgorithmVersion string             `json:"algorithmVersion"`
	MaxCCDStepDeg    float64            `json:"maxCCDStepDeg,omitempty"`
}

func DefaultParams() Params {
	return Params{
		ParamVersion:     1,
		AlgorithmVersion: DefaultAlgorithmVersion,
		MaxCCDStepDeg:    30,
	}
}

type Snapshot struct {
	SkeletonHash      string             `json:"skeletonHash"`
	MappingHash       string             `json:"mappingHash"`
	ParamVersion      int                `json:"paramVersion"`
	AlgorithmVersion  string             `json:"algorithmVersion"`
	BodyScale         float64            `json:"bodyScale"`
	ScaleCoefficients map[string]float64 `json:"scaleCoefficients"`
	Mapping           model.Mapping      `json:"mapping"`
}

type JointWorld struct {
	Joint    string    `json:"joint"`
	Position geom.Vec3 `json:"position"`
	Rotation geom.Quat `json:"rotation"`
}

type ConstraintStats struct {
	AngleViolationDeg    float64 `json:"angleViolationDeg"`
	MaxAngleViolationDeg float64 `json:"maxAngleViolationDeg"`
	AngleViolations      int     `json:"angleViolations"`
	EffectorViolation    float64 `json:"effectorViolation"`
	EffectorViolationSum float64 `json:"effectorViolationSum"`
	EffectorConstraints  int     `json:"effectorConstraints"`
	Iterations           int     `json:"iterations"`
}

type FrameResult struct {
	Frame           int             `json:"frame"`
	TargetRotations []geom.Quat     `json:"targetRotations"`
	World           []JointWorld    `json:"world"`
	Constraint      ConstraintStats `json:"constraint"`
	RootTrajectory  geom.Vec3       `json:"rootTrajectory"`
	RootShift       geom.Vec3       `json:"rootShift"`
	Contacts        []string        `json:"contacts"`
	Snapshot        Snapshot        `json:"snapshot"`
	CacheKey        string          `json:"cacheKey"`
}

func (r FrameResult) CanonicalJSON() ([]byte, error) {
	return json.Marshal(r)
}
