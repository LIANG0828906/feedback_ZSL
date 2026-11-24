package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func (s *Skeleton) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "skeleton", "name", "joints"); err != nil {
		return err
	}
	type alias Skeleton
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*s = Skeleton(decoded)
	return nil
}

func (j *Joint) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var name string
	if v, ok := raw["name"]; !ok {
		return fmt.Errorf("joint entry is missing required field name")
	} else if err := json.Unmarshal(v, &name); err != nil {
		return fmt.Errorf("joint name is invalid: %w", err)
	}
	if _, ok := raw["parent"]; !ok {
		return fmt.Errorf("joint %q is missing required field parent", name)
	}
	offset, ok := raw["localOffset"]
	if !ok {
		return fmt.Errorf("joint %q is missing required field localOffset", name)
	}
	if err := requireXYZ(offset, "joint "+name+" localOffset"); err != nil {
		return err
	}
	type jointAlias Joint
	var decoded jointAlias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*j = Joint(decoded)
	return nil
}

func (c *Clip) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "clip", "name", "frames", "contacts", "phases"); err != nil {
		return err
	}
	type alias Clip
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*c = Clip(decoded)
	return nil
}

func (f *Frame) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	frameLabel := "frame entry"
	if v, ok := raw["index"]; ok {
		var idx int
		if err := json.Unmarshal(v, &idx); err == nil {
			frameLabel = fmt.Sprintf("frame %d", idx)
		}
	} else {
		return fmt.Errorf("frame entry is missing required field index")
	}
	root, ok := raw["rootTranslation"]
	if !ok {
		return fmt.Errorf("%s is missing required field rootTranslation", frameLabel)
	}
	if err := requireXYZ(root, frameLabel+" rootTranslation"); err != nil {
		return err
	}
	rots, ok := raw["localRotations"]
	if !ok {
		return fmt.Errorf("%s is missing required field localRotations", frameLabel)
	}
	if bytes.Equal(bytes.TrimSpace(rots), []byte("null")) {
		return fmt.Errorf("%s localRotations must be an array, not null", frameLabel)
	}
	var rawRots []map[string]json.RawMessage
	if err := json.Unmarshal(rots, &rawRots); err != nil {
		return fmt.Errorf("%s localRotations is invalid: %w", frameLabel, err)
	}
	for i, q := range rawRots {
		for _, key := range []string{"w", "x", "y", "z"} {
			if _, ok := q[key]; !ok {
				return fmt.Errorf("%s localRotations[%d] is missing field %s", frameLabel, i, key)
			}
		}
	}
	type frameAlias Frame
	var decoded frameAlias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*f = Frame(decoded)
	return nil
}

func (c *ContactInterval) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "contact", "foot", "start", "end"); err != nil {
		return err
	}
	type alias ContactInterval
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*c = ContactInterval(decoded)
	return nil
}

func (p *Phase) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "phase", "index", "startFrame", "endFrame", "name"); err != nil {
		return err
	}
	type alias Phase
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*p = Phase(decoded)
	return nil
}

func (r *JointRule) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "mapping rule", "source", "targets"); err != nil {
		return err
	}
	type alias JointRule
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*r = JointRule(decoded)
	return nil
}

func (r *MissingRule) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "missing rule", "joint", "policy"); err != nil {
		return err
	}
	type alias MissingRule
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*r = MissingRule(decoded)
	return nil
}

func (r *EffectorRule) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "effector rule", "target", "source", "tolerance", "maxIterations"); err != nil {
		return err
	}
	type alias EffectorRule
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*r = EffectorRule(decoded)
	return nil
}

func (s *ScaleCoefficient) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "scale coefficient", "key", "value"); err != nil {
		return err
	}
	type alias ScaleCoefficient
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*s = ScaleCoefficient(decoded)
	return nil
}

func (m *Mapping) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "mapping", "name", "rules"); err != nil {
		return err
	}
	type alias Mapping
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*m = Mapping(decoded)
	return nil
}

func (t *TargetVariant) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "target", "name", "skeleton", "scale"); err != nil {
		return err
	}
	type alias TargetVariant
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*t = TargetVariant(decoded)
	return nil
}

func (w *Workspace) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "workspace", "seed", "source", "clip", "targets", "mapping", "truths"); err != nil {
		return err
	}
	type alias Workspace
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*w = Workspace(decoded)
	return nil
}

func (t *TargetTruth) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "truth", "target", "tolerance", "keyFrames", "sourceBoneLengths", "boneLengths", "contacts"); err != nil {
		return err
	}
	type alias TargetTruth
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*t = TargetTruth(decoded)
	return nil
}

func (f *TruthFrame) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "truth frame", "frame", "world", "root", "contacts"); err != nil {
		return err
	}
	type alias TruthFrame
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*f = TruthFrame(decoded)
	return nil
}

func (j *TruthJoint) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "truth joint", "joint", "position"); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	var jointName string
	_ = json.Unmarshal(raw["joint"], &jointName)
	if err := requireXYZ(raw["position"], "truth joint "+jointName+" position"); err != nil {
		return err
	}
	type alias TruthJoint
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*j = TruthJoint(decoded)
	return nil
}

func requireFields(b []byte, label string, names ...string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", label, err)
	}
	for _, name := range names {
		if _, ok := raw[name]; !ok {
			return fmt.Errorf("%s is missing required field %s", label, name)
		}
	}
	return nil
}

func requireXYZ(raw json.RawMessage, label string) error {
	var v map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%s is invalid: %w", label, err)
	}
	for _, key := range []string{"x", "y", "z"} {
		if _, ok := v[key]; !ok {
			return fmt.Errorf("%s is missing field %s", label, key)
		}
	}
	return nil
}
