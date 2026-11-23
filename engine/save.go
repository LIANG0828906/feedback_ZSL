package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"storyworkbench/story"
)

type SaveState struct {
	ChoiceOffset        int                       `json:"choice_offset"`
	ShotProgress        int                       `json:"shot_progress"`
	Shots               []ShotPresentation        `json:"shots"`
	NodeStates          map[string]NodeRuntime    `json:"node_states"`
	Flags               map[string]any            `json:"flags"`
	Characters          map[string]map[string]any `json:"characters"`
	EventOrder          []string                  `json:"event_order"`
	BlockedChoices      []BlockedChoice           `json:"blocked_choices"`
	ClockMS             int64                     `json:"clock_ms"`
	CurrentNodeID       string                    `json:"current_node_id"`
	CurrentNodeNextShot int                       `json:"current_node_next_shot"`
	CurrentChoiceIndex  int                       `json:"current_choice_index"`
	CurrentChoiceAtMS   int64                     `json:"current_choice_at_ms"`
	ActivationCounter   int                       `json:"activation_counter"`
}

type Save struct {
	SchemaVersion    int       `json:"schema_version"`
	AlgorithmVersion string    `json:"algorithm_version"`
	ProjectID        string    `json:"project_id"`
	ContentHash      string    `json:"content_hash"`
	Fingerprint      string    `json:"fingerprint"`
	Done             bool      `json:"done"`
	State            SaveState `json:"state"`
}

func (s *Session) Snapshot() Save {
	state := SaveState{
		ChoiceOffset:        s.state.ChoiceOffset,
		ShotProgress:        len(s.state.Shots),
		Shots:               cloneShots(s.state.Shots),
		NodeStates:          cloneNodeStates(s.state.NodeStates),
		Flags:               story.CloneAnyMap(s.state.Flags),
		Characters:          story.CloneCharacterState(s.state.Characters),
		EventOrder:          append([]string(nil), s.state.EventOrder...),
		BlockedChoices:      append([]BlockedChoice(nil), s.state.BlockedChoices...),
		ClockMS:             s.state.ClockMS,
		CurrentNodeID:       s.state.CurrentNodeID,
		CurrentNodeNextShot: s.state.CurrentNodeNextShot,
		CurrentChoiceIndex:  s.state.CurrentChoiceIndex,
		CurrentChoiceAtMS:   s.state.CurrentChoiceAtMS,
		ActivationCounter:   s.state.ActivationCounter,
	}
	save := Save{
		SchemaVersion:    SaveSchemaVersion,
		AlgorithmVersion: AlgorithmVersion,
		ProjectID:        s.project.ID,
		ContentHash:      s.contentHash,
		Done:             s.finished,
		State:            state,
	}
	save.Fingerprint = computeSaveFingerprint(&save)
	return save
}

func cloneShots(in []ShotPresentation) []ShotPresentation {
	out := make([]ShotPresentation, len(in))
	for i, shot := range in {
		out[i] = shot
		out[i].AspectPriority = append([]string(nil), shot.AspectPriority...)
		if shot.OriginalSequence != nil {
			seq := *shot.OriginalSequence
			out[i].OriginalSequence = &seq
		}
	}
	return out
}

func cloneNodeStates(in map[string]NodeRuntime) map[string]NodeRuntime {
	out := make(map[string]NodeRuntime, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func computeSaveFingerprint(save *Save) string {
	payload := struct {
		SchemaVersion    int       `json:"schema_version"`
		AlgorithmVersion string    `json:"algorithm_version"`
		ProjectID        string    `json:"project_id"`
		ContentHash      string    `json:"content_hash"`
		Done             bool      `json:"done"`
		State            SaveState `json:"state"`
	}{
		SchemaVersion:    save.SchemaVersion,
		AlgorithmVersion: save.AlgorithmVersion,
		ProjectID:        save.ProjectID,
		ContentHash:      save.ContentHash,
		Done:             save.Done,
		State:            save.State,
	}
	return story.HashJSON(payload)
}

func MarshalSave(save Save) ([]byte, error) {
	return json.MarshalIndent(save, "", "  ")
}

func WriteSaveFile(path string, save Save) error {
	data, err := MarshalSave(save)
	if err != nil {
		return story.NewError(story.CodeSave, "", "json", err.Error())
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return story.NewError(story.CodeSave, "", "file", err.Error())
	}
	tmp, err := os.CreateTemp(dir, ".save-*")
	if err != nil {
		return story.NewError(story.CodeSave, "", "file", err.Error())
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return story.NewError(story.CodeSave, "", "file", err.Error())
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return story.NewError(story.CodeSave, "", "file", err.Error())
	}
	if err := tmp.Close(); err != nil {
		return story.NewError(story.CodeSave, "", "file", err.Error())
	}
	if err := os.Rename(tmpName, path); err != nil {
		return story.NewError(story.CodeSave, "", "file", err.Error())
	}
	return nil
}

func LoadSaveFile(path string) (*Save, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "file", err.Error())
	}
	return ParseSave(data)
}

// ParseSave decodes and validates a save without a project. Project-specific
// content hash checks are performed by ValidateSave.
func ParseSave(data []byte) (*Save, error) {
	save, err := decodeSave(data)
	if err != nil {
		return nil, err
	}
	if err := validateSaveStructure(save); err != nil {
		return nil, err
	}
	if save.Fingerprint != computeSaveFingerprint(save) {
		return nil, story.NewError(story.CodeSave, save.State.CurrentNodeID, "fingerprint", "save content fingerprint mismatch")
	}
	return save, nil
}

func decodeSave(data []byte) (*Save, error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, story.NewError(story.CodeSave, "", "json", err.Error())
	}
	if err := ensureDecoderEOF(dec); err != nil {
		return nil, story.NewError(story.CodeSave, "", "json", err.Error())
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, story.NewError(story.CodeSave, "", "save", "save must be a JSON object")
	}
	for _, field := range []string{"schema_version", "algorithm_version", "project_id", "content_hash", "fingerprint", "done", "state"} {
		if _, ok := obj[field]; !ok {
			return nil, story.NewError(story.CodeSave, "", field, "required save field is missing")
		}
	}
	stateRaw, ok := obj["state"].(map[string]any)
	if !ok {
		return nil, story.NewError(story.CodeSave, "", "state", "must be an object")
	}
	for _, field := range []string{
		"choice_offset", "shot_progress", "shots", "node_states", "flags",
		"characters", "event_order", "blocked_choices", "clock_ms",
		"current_node_id", "current_node_next_shot", "current_choice_index",
		"current_choice_at_ms", "activation_counter",
	} {
		if _, ok := stateRaw[field]; !ok {
			return nil, story.NewError(story.CodeSave, "", "state."+field, "required save field is missing")
		}
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var save Save
	if err := dec.Decode(&save); err != nil {
		return nil, story.NewError(story.CodeSave, "", "json", err.Error())
	}
	normalizeSave(&save)
	return &save, nil
}

func ensureDecoderEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	if strings.Contains(err.Error(), "EOF") {
		return nil
	}
	return err
}

func normalizeSave(save *Save) {
	for k, v := range save.State.Flags {
		save.State.Flags[k] = story.NormalizeValue(v)
	}
	for id, state := range save.State.Characters {
		for k, v := range state {
			state[k] = story.NormalizeValue(v)
		}
		save.State.Characters[id] = state
	}
}

func validateSaveStructure(save *Save) error {
	if save.SchemaVersion != SaveSchemaVersion {
		return story.NewError(story.CodeSave, "", "schema_version", fmt.Sprintf("unsupported save schema version %d", save.SchemaVersion))
	}
	if save.AlgorithmVersion != AlgorithmVersion {
		return story.NewError(story.CodeSave, "", "algorithm_version", "save algorithm version does not match")
	}
	if save.ProjectID == "" {
		return story.NewError(story.CodeSave, "", "project_id", "project id is required")
	}
	if save.ContentHash == "" {
		return story.NewError(story.CodeSave, "", "content_hash", "content hash is required")
	}
	if save.Fingerprint == "" {
		return story.NewError(story.CodeSave, "", "fingerprint", "fingerprint is required")
	}
	if save.State.ChoiceOffset < 0 {
		return story.NewError(story.CodeSave, "", "state.choice_offset", "must be non-negative")
	}
	if save.State.ShotProgress != len(save.State.Shots) {
		return story.NewError(story.CodeSave, "", "state.shot_progress", "does not match stored shot count")
	}
	if save.State.ClockMS < 0 {
		return story.NewError(story.CodeSave, "", "state.clock_ms", "must be non-negative")
	}
	if save.State.CurrentNodeNextShot < 0 {
		return story.NewError(story.CodeSave, "", "state.current_node_next_shot", "must be non-negative")
	}
	if save.State.CurrentNodeID == "" && save.State.CurrentNodeNextShot != 0 {
		return story.NewError(story.CodeSave, "", "state.current_node_next_shot", "must be zero when no node is active")
	}
	if save.State.CurrentNodeID != "" && save.State.CurrentChoiceIndex < 0 {
		return story.NewError(story.CodeSave, save.State.CurrentNodeID, "state.current_choice_index", "must be non-negative")
	}
	var end int64
	for i, shot := range save.State.Shots {
		if shot.Sequence != i {
			return story.NewError(story.CodeSave, shot.NodeID, "state.shots.sequence", fmt.Sprintf("expected shot sequence %d, got %d", i, shot.Sequence))
		}
		if shot.DurationMS <= 0 {
			return story.NewError(story.CodeSave, shot.NodeID, "state.shots.duration_ms", "duration must be positive")
		}
		if shot.PresentedAtMS < end {
			return story.NewError(story.CodeSave, shot.NodeID, "state.shots.presented_at_ms", "shot boundaries overlap or are out of order")
		}
		if len(shot.AspectPriority) == 0 {
			return story.NewError(story.CodeSave, shot.NodeID, "state.shots.aspect_priority", "aspect priority is required")
		}
		end = shot.PresentedAtMS + shot.DurationMS
	}
	if end > save.State.ClockMS {
		return story.NewError(story.CodeSave, "", "state.clock_ms", "clock predates the last presented shot")
	}
	for id, rt := range save.State.NodeStates {
		if id == "" {
			return story.NewError(story.CodeSave, "", "state.node_states", "empty node id")
		}
		if rt.Completed && !rt.Activated {
			return story.NewError(story.CodeSave, id, "state.node_states.completed", "completed node must be activated")
		}
	}
	for i, blocked := range save.State.BlockedChoices {
		if blocked.ChoiceIndex < 0 {
			return story.NewError(story.CodeSave, blocked.Target, fmt.Sprintf("state.blocked_choices[%d].choice_index", i), "must be non-negative")
		}
	}
	return nil
}

func ValidateSave(project *story.Project, save *Save, choices []story.Choice) error {
	if save == nil {
		return story.NewError(story.CodeSave, "", "save", "save is nil")
	}
	if err := validateSaveStructure(save); err != nil {
		return err
	}
	if save.SchemaVersion != story.ProjectSchemaVersion {
		return story.NewError(story.CodeSave, "", "schema_version", "save structure version is incompatible")
	}
	if save.AlgorithmVersion != AlgorithmVersion {
		return story.NewError(story.CodeSave, "", "algorithm_version", "save algorithm version is incompatible")
	}
	if save.ProjectID != project.ID {
		return story.NewError(story.CodeSave, save.State.CurrentNodeID, "project_id", "save belongs to a different project")
	}
	expectedHash := story.ContentHash(project)
	if save.ContentHash != expectedHash {
		return story.NewError(story.CodeSave, save.State.CurrentNodeID, "content_hash", "save content hash does not match the project")
	}
	if save.Fingerprint != computeSaveFingerprint(save) {
		return story.NewError(story.CodeSave, save.State.CurrentNodeID, "fingerprint", "save content fingerprint mismatch")
	}
	if choices == nil {
		choices = project.Choices
	}
	if err := validateChoiceView(project, choices); err != nil {
		return story.NewError(story.CodeSave, "", "choices", err.Error())
	}
	if save.State.ChoiceOffset > len(choices) {
		return story.NewError(story.CodeSave, "", "state.choice_offset", "choice offset exceeds the available choice sequence")
	}
	nodeIndex := map[string]*story.Node{}
	for i := range project.Nodes {
		nodeIndex[project.Nodes[i].ID] = &project.Nodes[i]
	}
	for id := range save.State.NodeStates {
		if _, ok := nodeIndex[id]; !ok {
			return story.NewError(story.CodeSave, id, "state.node_states", "save references an unknown node")
		}
	}
	var previousEnd int64
	for i, shot := range save.State.Shots {
		node := nodeIndex[shot.NodeID]
		if node == nil {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].node_id", i), "unknown node")
		}
		if shot.NodeShotIndex < 0 || shot.NodeShotIndex >= len(node.Shots) {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].node_shot_index", i), "node shot index is out of range")
		}
		def := node.Shots[shot.NodeShotIndex]
		if shot.DurationMS != def.DurationMS {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].duration_ms", i), "duration does not match project")
		}
		if !sameStrings(shot.AspectPriority, def.AspectPriority) {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].aspect_priority", i), "aspect priority does not match project")
		}
		if i > 0 && shot.PresentedAtMS < previousEnd {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].presented_at_ms", i), "shot presentation overlaps the previous shot")
		}
		if shot.ContentHash != expectedHash {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].content_hash", i), "shot content hash mismatch")
		}
		if shot.ChoiceIndex < 0 || shot.ChoiceIndex >= len(choices) {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].choice_index", i), "choice index is out of range")
		}
		if choices[shot.ChoiceIndex].Target != shot.NodeID {
			return story.NewError(story.CodeSave, shot.NodeID, fmt.Sprintf("state.shots[%d].choice_index", i), "choice target does not match shot node")
		}
		previousEnd = shot.PresentedAtMS + shot.DurationMS
	}
	if len(save.State.Shots) > 0 && save.State.ClockMS != previousEnd {
		return story.NewError(story.CodeSave, save.State.CurrentNodeID, "state.clock_ms", "clock must equal the end of the last presented shot")
	}
	if len(save.State.Shots) == 0 && save.State.ClockMS != 0 {
		return story.NewError(story.CodeSave, save.State.CurrentNodeID, "state.clock_ms", "initial save clock must be zero")
	}
	if save.State.CurrentNodeID != "" {
		node := nodeIndex[save.State.CurrentNodeID]
		if node == nil {
			return story.NewError(story.CodeSave, save.State.CurrentNodeID, "state.current_node_id", "unknown node")
		}
		runtime := save.State.NodeStates[node.ID]
		if !runtime.Activated || runtime.Completed {
			return story.NewError(story.CodeSave, node.ID, "state.current_node_id", "current node must be active and incomplete")
		}
		if save.State.CurrentNodeNextShot >= len(node.Shots) {
			return story.NewError(story.CodeSave, node.ID, "state.current_node_next_shot", "current node has no remaining shots")
		}
		if save.State.CurrentChoiceIndex >= len(choices) {
			return story.NewError(story.CodeSave, node.ID, "state.current_choice_index", "choice index is out of range")
		}
		if choices[save.State.CurrentChoiceIndex].Target != node.ID {
			return story.NewError(story.CodeSave, node.ID, "state.current_choice_index", "active choice target does not match current node")
		}
		if save.State.CurrentChoiceAtMS != choices[save.State.CurrentChoiceIndex].AtMS {
			return story.NewError(story.CodeSave, node.ID, "state.current_choice_at_ms", "choice timestamp does not match the choice sequence")
		}
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func SessionFromSave(project *story.Project, save *Save, choices []story.Choice) (*Session, error) {
	if err := story.ValidateProject(project); err != nil {
		return nil, err
	}
	if err := ValidateSave(project, save, choices); err != nil {
		return nil, err
	}
	if choices == nil {
		choices = project.Choices
	}
	graph, err := story.BuildDependencyGraph(project)
	if err != nil {
		return nil, err
	}
	peers := make(map[string][]string, len(project.Nodes))
	for _, n := range project.Nodes {
		for _, m := range n.Mutex {
			peers[n.ID] = appendUnique(peers[n.ID], m)
			peers[m] = appendUnique(peers[m], n.ID)
		}
	}
	for id := range peers {
		sort.Strings(peers[id])
	}
	nodeStates := cloneNodeStates(save.State.NodeStates)
	for _, n := range project.Nodes {
		if _, ok := nodeStates[n.ID]; !ok {
			nodeStates[n.ID] = NodeRuntime{}
		}
	}
	s := &Session{
		project:     project,
		choices:     choices,
		graph:       graph,
		contentHash: story.ContentHash(project),
		mutexPeers:  peers,
		state: runtimeState{
			Flags:               story.CloneAnyMap(save.State.Flags),
			Characters:          story.CloneCharacterState(save.State.Characters),
			NodeStates:          nodeStates,
			EventOrder:          append([]string(nil), save.State.EventOrder...),
			BlockedChoices:      append([]BlockedChoice(nil), save.State.BlockedChoices...),
			Shots:               cloneShots(save.State.Shots),
			ChoiceOffset:        save.State.ChoiceOffset,
			ClockMS:             save.State.ClockMS,
			CurrentNodeID:       save.State.CurrentNodeID,
			CurrentNodeNextShot: save.State.CurrentNodeNextShot,
			CurrentChoiceIndex:  save.State.CurrentChoiceIndex,
			CurrentChoiceAtMS:   save.State.CurrentChoiceAtMS,
			ActivationCounter:   save.State.ActivationCounter,
		},
		finished: save.Done,
	}
	return s, nil
}

func Resume(project *story.Project, save *Save, choices []story.Choice) (*RunResult, error) {
	session, err := SessionFromSave(project, save, choices)
	if err != nil {
		return nil, err
	}
	for {
		_, ok, err := session.NextShot()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
	}
	return session.Result(), nil
}
