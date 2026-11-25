package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type SampleSet struct {
	Dir      string
	Manifest Manifest
	Clips    map[string]Clip
	Targets  map[string]Skeleton
	Truth    map[string]Truth
}

func LoadSampleSet(dir string) (*SampleSet, *Error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, &Error{Code: ECodeSchema, Message: "cannot read manifest: " + err.Error()}
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, &Error{Code: ECodeSchema, Message: "invalid manifest JSON: " + err.Error()}
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, NewError(ECodeSchema, "unsupported manifest schema_version: "+manifest.SchemaVersion)
	}
	set := &SampleSet{Dir: dir, Manifest: manifest, Clips: map[string]Clip{}, Targets: map[string]Skeleton{}, Truth: map[string]Truth{}}

	for _, decl := range manifest.Clips {
		path := filepath.Join(dir, decl.File)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &Error{Code: ECodeSchema, Message: "declared clip missing: " + decl.File}
		}
		if e := verifyHash(decl, data); e != nil {
			return nil, e
		}
		clip, e := DecodeAndValidateClip(data)
		if e != nil {
			return nil, e
		}
		if clip.ID != decl.ID {
			return nil, &Error{Code: ECodeSchema, Message: fmt.Sprintf("manifest clip id %s does not match content id %s", decl.ID, clip.ID)}
		}
		if decl.Frames != 0 && decl.Frames != len(clip.Frames) {
			return nil, &Error{Code: ECodeSchema, Message: "manifest frame count does not match clip: " + decl.ID}
		}
		if decl.Joints != 0 && decl.Joints != len(clip.Skeleton.Joints) {
			return nil, &Error{Code: ECodeSchema, Joint: "", Message: "manifest joint count does not match clip: " + decl.ID}
		}
		set.Clips[clip.ID] = clip
	}
	for _, decl := range manifest.Targets {
		data, err := os.ReadFile(filepath.Join(dir, decl.File))
		if err != nil {
			return nil, &Error{Code: ECodeSchema, Message: "declared target missing: " + decl.File}
		}
		if e := verifyHash(decl, data); e != nil {
			return nil, e
		}
		target, e := DecodeAndValidateSkeleton(data)
		if e != nil {
			return nil, e
		}
		if target.ID != decl.ID {
			return nil, &Error{Code: ECodeSchema, Message: "manifest target id does not match content: " + decl.ID}
		}
		if decl.Joints != 0 && decl.Joints != len(target.Joints) {
			return nil, &Error{Code: ECodeSchema, Message: "manifest joint count does not match target: " + decl.ID}
		}
		set.Targets[target.ID] = target
	}
	for _, decl := range manifest.Truth {
		data, err := os.ReadFile(filepath.Join(dir, decl.File))
		if err != nil {
			return nil, &Error{Code: ECodeSchema, Message: "declared truth missing: " + decl.File}
		}
		if e := verifyHash(decl, data); e != nil {
			return nil, e
		}
		var truth Truth
		if err := json.Unmarshal(data, &truth); err != nil {
			return nil, &Error{Code: ECodeSchema, Message: "invalid truth JSON: " + err.Error()}
		}
		if truth.SchemaVersion != SchemaVersion {
			return nil, NewError(ECodeSchema, "unsupported truth schema_version")
		}
		if truth.ClipID != decl.ID {
			return nil, &Error{Code: ECodeSchema, Message: "manifest truth id does not match clip_id: " + decl.ID}
		}
		set.Truth[truth.ClipID] = truth
	}
	return set, nil
}

func verifyHash(decl AssetDecl, data []byte) *Error {
	if decl.SHA256 == "" {
		return &Error{Code: ECodeSchema, Message: "manifest asset missing sha256: " + decl.ID}
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if actual != decl.SHA256 {
		return &Error{Code: ECodeSchema, Message: "sha256 declaration mismatch for " + decl.ID, Details: map[string]string{"expected": decl.SHA256, "actual": actual}}
	}
	return nil
}

func DecodeAndValidateClip(data []byte) (Clip, *Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Clip{}, &Error{Code: ECodeSchema, Message: "invalid clip JSON: " + err.Error()}
	}
	if e := requireKeys(raw, "schema_version", "id", "seed", "frames_count", "skeleton", "frames", "contact_tracks"); e != nil {
		return Clip{}, e
	}
	if e := requireSkeletonKeys(raw["skeleton"]); e != nil {
		return Clip{}, e
	}
	if e := requireClipFrameKeys(raw["frames"]); e != nil {
		return Clip{}, e
	}
	if e := requireContactTrackKeys(raw["contact_tracks"]); e != nil {
		return Clip{}, e
	}
	var clip Clip
	if err := json.Unmarshal(data, &clip); err != nil {
		return Clip{}, &Error{Code: ECodeSchema, Message: "cannot decode clip: " + err.Error()}
	}
	if e := ValidateClip(clip); e != nil {
		return Clip{}, e
	}
	return clip, nil
}

func LoadClipFile(path string) (Clip, *Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Clip{}, &Error{Code: ECodeSchema, Message: err.Error()}
	}
	return DecodeAndValidateClip(data)
}

func DecodeAndValidateSkeleton(data []byte) (Skeleton, *Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Skeleton{}, &Error{Code: ECodeSchema, Message: "invalid skeleton JSON: " + err.Error()}
	}
	if e := requireKeys(raw, "id", "joints"); e != nil {
		return Skeleton{}, e
	}
	if e := requireSkeletonKeys(data); e != nil {
		return Skeleton{}, e
	}
	var sk Skeleton
	if err := json.Unmarshal(data, &sk); err != nil {
		return Skeleton{}, &Error{Code: ECodeSchema, Message: "cannot decode skeleton: " + err.Error()}
	}
	if e := finiteSkeleton(sk); e != nil {
		return Skeleton{}, e
	}
	if _, e := NewSkeletonIndex(sk); e != nil {
		return Skeleton{}, e
	}
	return sk, nil
}

func LoadSkeletonFile(path string) (Skeleton, *Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skeleton{}, &Error{Code: ECodeSchema, Message: err.Error()}
	}
	return DecodeAndValidateSkeleton(data)
}

func requireKeys(raw map[string]json.RawMessage, keys ...string) *Error {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok || string(v) == "null" {
			return &Error{Code: ECodeSchema, Field: key, Message: "missing required field: " + key}
		}
	}
	return nil
}

func requireSkeletonKeys(data json.RawMessage) *Error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return &Error{Code: ECodeSchema, Field: "skeleton", Message: "skeleton must be an object"}
	}
	if e := requireKeys(raw, "id", "joints"); e != nil {
		return e
	}
	var joints []json.RawMessage
	if err := json.Unmarshal(raw["joints"], &joints); err != nil {
		return &Error{Code: ECodeSchema, Field: "joints", Message: "joints must be an array"}
	}
	for i, jr := range joints {
		var joint map[string]json.RawMessage
		if err := json.Unmarshal(jr, &joint); err != nil {
			return &Error{Code: ECodeSchema, Field: "joints", Message: fmt.Sprintf("joint %d must be an object", i)}
		}
		if e := requireKeys(joint, "name", "parent", "offset"); e != nil {
			if e.Joint == "" {
				e.Details = map[string]int{"joint_index": i}
			}
			return e
		}
	}
	return nil
}

func requireClipFrameKeys(data json.RawMessage) *Error {
	var frames []json.RawMessage
	if err := json.Unmarshal(data, &frames); err != nil {
		return &Error{Code: ECodeSchema, Field: "frames", Message: "frames must be an array"}
	}
	for i, fr := range frames {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(fr, &frame); err != nil {
			return &Error{Code: ECodeSchema, Frame: i, Field: "frames", Message: "frame must be an object"}
		}
		if e := requireKeys(frame, "index", "root", "rotations"); e != nil {
			e.Frame = i
			return e
		}
	}
	return nil
}

func requireContactTrackKeys(data json.RawMessage) *Error {
	var tracks []json.RawMessage
	if err := json.Unmarshal(data, &tracks); err != nil {
		return &Error{Code: ECodeSchema, Field: "contact_tracks", Message: "contact_tracks must be an array"}
	}
	for i, tr := range tracks {
		var track map[string]json.RawMessage
		if err := json.Unmarshal(tr, &track); err != nil {
			return &Error{Code: ECodeSchema, Field: "contact_tracks", Message: "contact track must be an object"}
		}
		if e := requireKeys(track, "foot", "joint", "values"); e != nil {
			e.Details = map[string]int{"track_index": i}
			return e
		}
	}
	return nil
}

func ValidateClip(clip Clip) *Error {
	if clip.SchemaVersion != SchemaVersion {
		return &Error{Code: ECodeSchema, Field: "schema_version", Message: "unsupported schema_version: " + clip.SchemaVersion}
	}
	if clip.ID == "" {
		return &Error{Code: ECodeSchema, Field: "id", Message: "clip id is missing"}
	}
	if clip.FramesCount <= 0 {
		return &Error{Code: ECodeSchema, Field: "frames_count", Message: "frames_count must be positive"}
	}
	if len(clip.Frames) != clip.FramesCount {
		return &Error{Code: ECodeSchema, Field: "frames", Message: fmt.Sprintf("frames_count=%d but found %d frames", clip.FramesCount, len(clip.Frames))}
	}
	if e := finiteSkeleton(clip.Skeleton); e != nil {
		return e
	}
	index, e := NewSkeletonIndex(clip.Skeleton)
	if e != nil {
		return e
	}

	footSet := map[string]bool{}
	for _, name := range clip.Feet {
		ji, ok := index.ByName[name]
		if !ok {
			return &Error{Code: ECodeSchema, Joint: name, Message: "feet declaration references unknown joint"}
		}
		if !clip.Skeleton.Joints[ji].Foot {
			return &Error{Code: ECodeSchema, Joint: name, Message: "feet declaration references non-foot joint"}
		}
		footSet[name] = true
	}

	for fi, frame := range clip.Frames {
		if frame.Index != fi {
			return &Error{Code: ECodeSchema, Frame: fi, Field: "index", Message: fmt.Sprintf("frame index field is %d", frame.Index)}
		}
		if !frame.Root.Finite() {
			return &Error{Code: ECodeSchema, Frame: fi, Field: "root", Message: "root translation contains non-finite value"}
		}
		if len(frame.Rotations) != len(clip.Skeleton.Joints) {
			qe := &Error{
				Code:    ECodeQuat,
				Frame:   fi,
				Field:   "rotations",
				Message: fmt.Sprintf("quaternion count %d does not match joint count %d", len(frame.Rotations), len(clip.Skeleton.Joints)),
				Details: map[string]int{"quaternions": len(frame.Rotations), "joints": len(clip.Skeleton.Joints)},
			}
			if len(frame.Rotations) < len(clip.Skeleton.Joints) {
				qe.Joint = clip.Skeleton.Joints[len(frame.Rotations)].Name
				qe.Details = map[string]int{"quaternions": len(frame.Rotations), "joints": len(clip.Skeleton.Joints), "expected_index": len(frame.Rotations)}
			} else {
				qe.Joint = clip.Skeleton.Joints[len(clip.Skeleton.Joints)-1].Name
				qe.Details = map[string]int{"quaternions": len(frame.Rotations), "joints": len(clip.Skeleton.Joints), "first_extra_index": len(clip.Skeleton.Joints)}
			}
			return qe
		}
		for qi, q := range frame.Rotations {
			jointName := clip.Skeleton.Joints[qi].Name
			if !q.Finite() || q.Norm() < 1e-12 {
				return &Error{Code: ECodeQuat, Frame: fi, Joint: jointName, Message: "quaternion is non-finite or zero-length"}
			}
		}
		if len(frame.Contacts) != 0 && len(frame.Contacts) != len(clip.Feet) {
			return &Error{Code: ECodeSchema, Frame: fi, Field: "contacts", Message: "per-frame contact count does not match feet count"}
		}
	}

	if len(clip.ContactTracks) == 0 {
		return &Error{Code: ECodeSchema, Field: "contact_tracks", Message: "contact tracks are missing"}
	}
	for _, track := range clip.ContactTracks {
		if track.Foot == "" || track.Joint == "" {
			return &Error{Code: ECodeSchema, Field: "contact_tracks", Message: "contact track foot/joint name is missing"}
		}
		ji, ok := index.ByName[track.Joint]
		if !ok {
			return &Error{Code: ECodeSchema, Joint: track.Joint, Message: "contact track references unknown joint"}
		}
		if !clip.Skeleton.Joints[ji].Foot {
			return &Error{Code: ECodeSchema, Joint: track.Joint, Message: "contact track references a non-foot joint"}
		}
		if len(track.Values) != len(clip.Frames) {
			return &Error{Code: ECodeSchema, Joint: track.Joint, Field: "values", Message: fmt.Sprintf("contact track length %d does not match frame count %d", len(track.Values), len(clip.Frames))}
		}
		for _, in := range ExtractIntervals(track.Values) {
			if in[0] < 0 || in[1] >= len(clip.Frames) || in[0] > in[1] {
				return &Error{Code: ECodeSchema, Joint: track.Joint, Message: "contact interval falls outside legal frame range", Details: in}
			}
		}
	}
	_ = footSet
	return nil
}

type TruthReport struct {
	OK            bool     `json:"ok"`
	ClipID        string   `json:"clip_id"`
	MaxWorldError float64  `json:"max_world_error"`
	MaxBoneError  float64  `json:"max_bone_error"`
	RootError     float64  `json:"root_error"`
	Mismatches    []string `json:"mismatches"`
}

func VerifyTruth(clip Clip, truth Truth) TruthReport {
	report := TruthReport{OK: true, ClipID: clip.ID}
	if truth.ClipID != clip.ID {
		report.OK = false
		report.Mismatches = append(report.Mismatches, "truth clip_id mismatch")
	}
	if truth.Seed != clip.Seed {
		report.OK = false
		report.Mismatches = append(report.Mismatches, "truth seed mismatch")
	}
	for _, f := range truth.KeyFrames {
		if f < 0 || f >= len(clip.Frames) {
			report.OK = false
			report.Mismatches = append(report.Mismatches, fmt.Sprintf("truth keyframe %d out of range", f))
			continue
		}
		pose, e := ForwardKinematics(clip.Skeleton, clip.Frames[f].Root, clip.Frames[f].Rotations)
		if e != nil {
			report.OK = false
			report.Mismatches = append(report.Mismatches, e.Error())
			continue
		}
		declared, ok := truth.WorldPositions[fmt.Sprintf("%d", f)]
		if !ok {
			report.OK = false
			report.Mismatches = append(report.Mismatches, fmt.Sprintf("missing truth world positions for keyframe %d", f))
			continue
		}
		for i, j := range clip.Skeleton.Joints {
			want, ok := declared[j.Name]
			if !ok {
				report.OK = false
				report.Mismatches = append(report.Mismatches, fmt.Sprintf("truth missing joint %s at frame %d", j.Name, f))
				continue
			}
			d := pose.WorldPos[i].Sub(want).Norm()
			if d > report.MaxWorldError {
				report.MaxWorldError = d
			}
			if d > 1e-7 {
				report.OK = false
				report.Mismatches = append(report.Mismatches, fmt.Sprintf("world position mismatch frame=%d joint=%s err=%.3g", f, j.Name, d))
			}
		}
	}
	for name, want := range truth.BoneLengths {
		got := clip.Skeleton.IndexBoneLength(name)
		d := absFloat(got - want)
		if d > report.MaxBoneError {
			report.MaxBoneError = d
		}
		if d > 1e-9 {
			report.OK = false
			report.Mismatches = append(report.Mismatches, "bone length mismatch: "+name)
		}
	}
	if len(truth.RootTrajectory) != len(clip.Frames) {
		report.OK = false
		report.Mismatches = append(report.Mismatches, "root trajectory length mismatch")
	} else {
		for f := range clip.Frames {
			d := clip.Frames[f].Root.Sub(truth.RootTrajectory[f]).Norm()
			if d > report.RootError {
				report.RootError = d
			}
		}
		if report.RootError > 1e-9 {
			report.OK = false
			report.Mismatches = append(report.Mismatches, "root trajectory mismatch")
		}
	}
	for foot, intervals := range truth.ContactIntervals {
		var track *ContactTrack
		for i := range clip.ContactTracks {
			if clip.ContactTracks[i].Foot == foot {
				track = &clip.ContactTracks[i]
			}
		}
		if track == nil {
			report.OK = false
			report.Mismatches = append(report.Mismatches, "truth contact foot missing: "+foot)
			continue
		}
		got := ExtractIntervals(track.Values)
		if len(got) != len(intervals) {
			report.OK = false
			report.Mismatches = append(report.Mismatches, "contact interval count mismatch: "+foot)
			continue
		}
		for i := range got {
			if got[i] != intervals[i] {
				report.OK = false
				report.Mismatches = append(report.Mismatches, fmt.Sprintf("contact interval mismatch %s %v vs %v", foot, got[i], intervals[i]))
			}
		}
	}
	return report
}

func (sk Skeleton) IndexBoneLength(name string) float64 {
	for _, j := range sk.Joints {
		if j.Name == name {
			return j.Offset.Norm()
		}
	}
	return 0
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
