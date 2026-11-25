package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func LoadSampleSet(path string) (*SampleSet, *KernelError) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, Errorf(CodeDecode, path, "$", "cannot read sample: %v", err)
	}
	return DecodeSampleSet(bytes.NewReader(payload), path)
}

func DecodeSampleSet(reader io.Reader, source string) (*SampleSet, *KernelError) {
	decoder := json.NewDecoder(reader)
	var set SampleSet
	if err := decoder.Decode(&set); err != nil {
		return nil, Errorf(CodeDecode, source, "$", "invalid JSON: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, Errorf(CodeDecode, source, "$", "multiple JSON values are not allowed")
		}
		return nil, Errorf(CodeDecode, source, "$", "invalid trailing JSON: %v", err)
	}
	if len(set.Sessions) == 0 {
		return nil, Errorf(CodeSchema, source, "$.sessions", "at least one session is required")
	}
	for i := range set.Sessions {
		if err := ValidateSession(&set.Sessions[i], source, fmt.Sprintf("$.sessions[%d]", i)); err != nil {
			return nil, err
		}
	}
	return &set, nil
}

func ValidateSession(session *Session, source, pathPrefix string) *KernelError {
	if session == nil {
		return Errorf(CodeSchema, source, pathPrefix, "session is nil")
	}
	if pathPrefix == "" {
		pathPrefix = "$"
	}
	if session.SchemaVersion != CurrentSchemaVersion {
		if session.SchemaVersion == 0 {
			return Errorf(CodeSchema, source, pathPrefix+".schema_version", "schema_version is required")
		}
		return Errorf(CodeSchema, source, pathPrefix+".schema_version", "unsupported schema_version %d", session.SchemaVersion)
	}
	if session.ID == "" {
		return Errorf(CodeSchema, source, pathPrefix+".session_id", "session_id is required")
	}
	if session.AlgorithmVersion == "" {
		return Errorf(CodeSchema, source, pathPrefix+".algorithm_version", "algorithm_version is required")
	}
	if session.Master.ID == "" {
		return Errorf(CodeSchema, source, pathPrefix+".master.id", "master id is required")
	}
	if err := ValidateEndpoint(session.Master, source, pathPrefix+".master"); err != nil {
		return locateDisplay(err, session.Master.ID)
	}
	if len(session.Displays) == 0 {
		return Errorf(CodeSchema, source, pathPrefix+".displays", "at least one controlled display is required")
	}

	seen := map[string]string{session.Master.ID: pathPrefix + ".master.id"}
	displayIndex := make(map[string]int, len(session.Displays))
	workingDisplays := append([]Endpoint(nil), session.Displays...)
	for i := range session.Displays {
		display := session.Displays[i]
		path := fmt.Sprintf("%s.displays[%d]", pathPrefix, i)
		if display.ID == "" {
			return Errorf(CodeSchema, source, path+".id", "display id is required")
		}
		if first, ok := seen[display.ID]; ok {
			return Errorf(CodeSchema, source, path+".id", "duplicate endpoint id %q (first at %s)", display.ID, first)
		}
		seen[display.ID] = path + ".id"
		displayIndex[display.ID] = i
		if err := ValidateEndpoint(display, source, path); err != nil {
			return locateDisplay(err, display.ID)
		}
	}

	initialSeen := make(map[string]bool, len(session.InitialDisplays))
	for i, id := range session.InitialDisplays {
		path := fmt.Sprintf("%s.initial_displays[%d]", pathPrefix, i)
		if id == "" {
			return Errorf(CodeSchema, source, path, "initial display id is required")
		}
		if _, ok := displayIndex[id]; !ok {
			return locateDisplay(Errorf(CodeRef, source, path, "unknown display %q", id), id)
		}
		if initialSeen[id] {
			return Errorf(CodeSchema, source, path, "duplicate initial display %q", id)
		}
		initialSeen[id] = true
	}

	var previousEventTime int64
	var previousSeq int64
	for i := range session.Events {
		event := &session.Events[i]
		path := fmt.Sprintf("%s.events[%d]", pathPrefix, i)
		if event.Seq <= 0 {
			return Errorf(CodeSchema, source, path+".seq", "seq must be positive")
		}
		if previousSeq != 0 && event.Seq <= previousSeq {
			return locateDisplay(Errorf(CodeOrder, source, path+".seq",
				"event seq must be strictly increasing: %d after %d", event.Seq, previousSeq), event.DisplayID)
		}
		previousSeq = event.Seq
		if event.Time < 0 {
			return Errorf(CodeSchema, source, path+".time", "time must not be negative")
		}
		if i > 0 && event.Time < previousEventTime {
			return locateDisplay(Errorf(CodeOrder, source, path+".time",
				"event time must be monotonic: %d after %d", event.Time, previousEventTime), event.DisplayID)
		}
		previousEventTime = event.Time
		switch event.Kind {
		case EventNegotiate, EventStart, EventPause, EventResume, EventEnd:
		case EventJoin, EventLeave, EventTopology:
			if event.DisplayID == "" {
				return Errorf(CodeSchema, source, path+".display_id", "display_id is required for %s", event.Kind)
			}
			index, ok := displayIndex[event.DisplayID]
			if !ok {
				return locateDisplay(Errorf(CodeRef, source, path+".display_id", "unknown display %q", event.DisplayID), event.DisplayID)
			}
			if event.Kind == EventTopology {
				if event.Topology == nil {
					return Errorf(CodeSchema, source, path+".topology", "topology is required for topology event")
				}
				capability := workingDisplays[index].Capability
				if event.Capability != nil {
					capability = *event.Capability
				}
				updated := Endpoint{
					ID:         event.DisplayID,
					Topology:   *event.Topology,
					Capability: capability,
				}
				if err := ValidateEndpoint(updated, source, path); err != nil {
					return locateDisplay(err, event.DisplayID)
				}
				workingDisplays[index] = updated
			}
		default:
			return Errorf(CodeSchema, source, path+".kind", "unknown event kind %q", event.Kind)
		}
	}

	if session.TotalWindows <= 0 {
		return Errorf(CodeSchema, source, pathPrefix+".total_windows", "total_windows must be positive")
	}
	if session.TotalWindows != len(session.Windows) {
		return Errorf(CodeSchema, source, pathPrefix+".total_windows", "total_windows %d does not match %d windows", session.TotalWindows, len(session.Windows))
	}
	var previousWindowTime int64
	for i := range session.Windows {
		window := &session.Windows[i]
		path := fmt.Sprintf("%s.windows[%d]", pathPrefix, i)
		if window.Index != i+1 {
			return Errorf(CodeSchema, source, path+".index", "window index must be %d, got %d", i+1, window.Index)
		}
		if window.Time < 0 {
			return Errorf(CodeSchema, source, path+".time", "time must not be negative")
		}
		if i > 0 && window.Time < previousWindowTime {
			err := Errorf(CodeOrder, source, path+".time",
				"window time must be monotonic: %d after %d", window.Time, previousWindowTime)
			err.Window = i + 1
			return err
		}
		previousWindowTime = window.Time
		if window.Region.Width <= 0 || window.Region.Height <= 0 {
			return Errorf(CodeSchema, source, path+".region", "region width and height must be positive")
		}
		seenTarget := make(map[string]bool, len(window.Targets))
		for j, target := range window.Targets {
			targetPath := fmt.Sprintf("%s.targets[%d]", path, j)
			if target == "" {
				return Errorf(CodeSchema, source, targetPath, "target display id is required")
			}
			if _, ok := displayIndex[target]; !ok {
				return locateDisplay(Errorf(CodeRef, source, targetPath, "unknown display %q", target), target)
			}
			if seenTarget[target] {
				return Errorf(CodeSchema, source, targetPath, "duplicate target %q", target)
			}
			seenTarget[target] = true
		}
		var previousInteractionTime int64
		var previousInteractionSeq int64
		for j := range window.Interactions {
			interaction := window.Interactions[j]
			interactionPath := fmt.Sprintf("%s.interactions[%d]", path, j)
			if interaction.Seq <= 0 {
				return Errorf(CodeSchema, source, interactionPath+".seq", "seq must be positive")
			}
			if previousInteractionSeq != 0 && interaction.Seq <= previousInteractionSeq {
				return Errorf(CodeOrder, source, interactionPath+".seq", "interaction seq must increase")
			}
			if interaction.Time < previousInteractionTime {
				return Errorf(CodeOrder, source, interactionPath+".time", "interaction time must be monotonic")
			}
			previousInteractionSeq = interaction.Seq
			previousInteractionTime = interaction.Time
		}
	}

	schedule := BuildSchedule(session)
	active := make(map[string]bool, len(session.InitialDisplays))
	for _, id := range session.InitialDisplays {
		active[id] = true
	}
	for _, action := range schedule {
		if action.Frame {
			window := session.Windows[action.WindowIndex]
			if len(window.Targets) == 0 {
				continue
			}
			for j, target := range window.Targets {
				if !active[target] {
					err := Errorf(CodeRef, source, fmt.Sprintf("%s.windows[%d].targets[%d]", pathPrefix, action.WindowIndex, j),
						"display %q is not joined at window time %d", target, window.Time)
					err.Window = action.WindowIndex + 1
					return locateDisplay(err, target)
				}
			}
			continue
		}
		event := session.Events[action.EventIndex]
		switch event.Kind {
		case EventJoin:
			active[event.DisplayID] = true
		case EventLeave:
			delete(active, event.DisplayID)
		}
	}
	return nil
}

func ValidateEndpoint(endpoint Endpoint, source, path string) *KernelError {
	if endpoint.ID == "" {
		return Errorf(CodeSchema, source, path+".id", "endpoint id is required")
	}
	topology := endpoint.Topology
	if topology.Width <= 0 {
		return Errorf(CodeSchema, source, path+".topology.width", "topology width must be positive")
	}
	if topology.Height <= 0 {
		return Errorf(CodeSchema, source, path+".topology.height", "topology height must be positive")
	}
	if topology.Resolution.Width <= 0 {
		return Errorf(CodeSchema, source, path+".topology.resolution.width", "resolution width must be positive")
	}
	if topology.Resolution.Height <= 0 {
		return Errorf(CodeSchema, source, path+".topology.resolution.height", "resolution height must be positive")
	}
	if !topology.Scale.Positive() {
		return Errorf(CodeSchema, source, path+".topology.scale", "topology scale must be positive")
	}
	capability := endpoint.Capability
	if capability.Resolution.Width <= 0 {
		return Errorf(CodeSchema, source, path+".capability.resolution.width", "capability resolution width must be positive")
	}
	if capability.Resolution.Height <= 0 {
		return Errorf(CodeSchema, source, path+".capability.resolution.height", "capability resolution height must be positive")
	}
	if !capability.Scale.Positive() {
		return Errorf(CodeSchema, source, path+".capability.scale", "capability scale must be positive")
	}
	if capability.CropMode == "" {
		return Errorf(CodeSchema, source, path+".capability.crop_mode", "crop_mode is required")
	}
	if !capability.CropMode.Valid() {
		return Errorf(CodeSchema, source, path+".capability.crop_mode", "unsupported crop_mode %q", capability.CropMode)
	}
	if capability.Resolution != topology.Resolution {
		return Errorf(CodeCap, source, path+".capability.resolution", "capability resolution %dx%d conflicts with topology resolution %dx%d",
			capability.Resolution.Width, capability.Resolution.Height,
			topology.Resolution.Width, topology.Resolution.Height)
	}
	if !capability.Scale.Equal(topology.Scale) {
		return Errorf(CodeCap, source, path+".capability.scale", "capability scale %s conflicts with topology scale %s",
			capability.Scale, topology.Scale)
	}
	expectedWidth := int64(capability.Resolution.Width) * capability.Scale.Den
	actualWidth := int64(topology.Width) * capability.Scale.Num
	if expectedWidth != actualWidth {
		return Errorf(CodeCap, source, path+".topology.width",
			"topology logical width %d is inconsistent with resolution %d and scale %s",
			topology.Width, capability.Resolution.Width, capability.Scale)
	}
	expectedHeight := int64(capability.Resolution.Height) * capability.Scale.Den
	actualHeight := int64(topology.Height) * capability.Scale.Num
	if expectedHeight != actualHeight {
		return Errorf(CodeCap, source, path+".topology.height",
			"topology logical height %d is inconsistent with resolution %d and scale %s",
			topology.Height, capability.Resolution.Height, capability.Scale)
	}
	return nil
}

func locateDisplay(err *KernelError, displayID string) *KernelError {
	if err != nil && err.DisplayID == "" {
		err.DisplayID = displayID
	}
	return err
}
