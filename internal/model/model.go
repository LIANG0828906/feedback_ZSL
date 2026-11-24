package model

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	ProjectSchemaV1   = "narrative-project/v1"
	AlgorithmVersion  = "deterministic-engine/v1.1.0"
	CheckpointVersion = "checkpoint/v1"
)

type ErrorCode string

const (
	CodeSchema  ErrorCode = "E_SCHEMA"
	CodeRef     ErrorCode = "E_REF"
	CodeCycle   ErrorCode = "E_REF"
	CodeChoice  ErrorCode = "E_CHOICE"
	CodeVersion ErrorCode = "E_VERSION"
	CodeHash    ErrorCode = "E_HASH"
	CodeLossy   ErrorCode = "E_LOSSY"
	CodeState   ErrorCode = "E_STATE"
)

type Error struct {
	Code         ErrorCode `json:"code"`
	Message      string    `json:"message"`
	Path         string    `json:"path,omitempty"`
	ConflictNode string    `json:"conflict_node,omitempty"`
	ConflictShot string    `json:"conflict_shot,omitempty"`
	Expected     string    `json:"expected,omitempty"`
	Actual       string    `json:"actual,omitempty"`
	LossyFields  []string  `json:"lossy_fields,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.ConflictNode != "" {
		return fmt.Sprintf("%s: %s (first conflict node: %s)", e.Code, e.Message, e.ConflictNode)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func schemaError(path, format string, args ...any) error {
	return &Error{Code: CodeSchema, Path: path, Message: fmt.Sprintf(format, args...)}
}

func refError(node, format string, args ...any) error {
	return &Error{Code: CodeRef, ConflictNode: node, Message: fmt.Sprintf(format, args...)}
}

type Project struct {
	Schema           string         `json:"schema"`
	ID               string         `json:"id"`
	Seed             int64          `json:"seed"`
	SampleIntervalMS int64          `json:"sample_interval_ms"`
	ToleranceMS      int64          `json:"tolerance_ms"`
	ObservableFlags  []string       `json:"observable_flags,omitempty"`
	Nodes            []Node         `json:"nodes"`
	ChoiceSequence   ChoiceSequence `json:"choice_sequence"`
	Truth            Truth          `json:"truth"`
}

type Node struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Parents       []string       `json:"parents,omitempty"`
	Preconditions Condition      `json:"preconditions,omitempty"`
	MutexWith     []string       `json:"mutex_with,omitempty"`
	Convergence   *Convergence   `json:"convergence,omitempty"`
	OnArrive      Effects        `json:"on_arrive,omitempty"`
	Options       []ChoiceOption `json:"options,omitempty"`
	Shots         []Shot         `json:"shots"`
	Layout        *Layout        `json:"layout,omitempty"`

	ShotsPresent bool `json:"-"`
}

func (n *Node) UnmarshalJSON(data []byte) error {
	type alias Node
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, decoded.ShotsPresent = fields["shots"]
	*n = Node(decoded)
	return nil
}

type Condition struct {
	All  []string `json:"all,omitempty"`
	Any  []string `json:"any,omitempty"`
	None []string `json:"none,omitempty"`
}

func (c Condition) IsEmpty() bool {
	return len(c.All) == 0 && len(c.Any) == 0 && len(c.None) == 0
}

type Convergence struct {
	ID           string   `json:"id,omitempty"`
	Policy       string   `json:"policy"`
	Predecessors []string `json:"predecessors"`
}

type Effects struct {
	SetFlags   []string          `json:"set_flags,omitempty"`
	ClearFlags []string          `json:"clear_flags,omitempty"`
	Characters []CharacterChange `json:"characters,omitempty"`
}

type CharacterChange struct {
	CharacterID string `json:"character_id"`
	Key         string `json:"key"`
	Value       string `json:"value"`
}

type ChoiceOption struct {
	ID      string  `json:"id"`
	Label   string  `json:"label,omitempty"`
	Effects Effects `json:"effects,omitempty"`
}

type ChoiceSequence struct {
	ID    string   `json:"id,omitempty"`
	Items []Choice `json:"items"`
}

type Choice struct {
	Index    int    `json:"index"`
	NodeID   string `json:"node_id"`
	OptionID string `json:"option_id"`
}

type Shot struct {
	ID         string `json:"id"`
	DurationMS int64  `json:"duration_ms"`
	AspectPath string `json:"aspect_path"`
	Camera     string `json:"camera,omitempty"`
	Motion     string `json:"motion,omitempty"`
	Transition string `json:"transition,omitempty"`

	DurationPresent bool `json:"-"`
}

func (s *Shot) UnmarshalJSON(data []byte) error {
	type alias Shot
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	rawDuration, exists := fields["duration_ms"]
	decoded.DurationPresent = exists && string(rawDuration) != "null"
	*s = Shot(decoded)
	return nil
}

type Layout struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Truth struct {
	TerminalFlags   map[string]bool `json:"terminal_flags"`
	EventOrder      []string        `json:"event_order"`
	ReachableNodes  []string        `json:"reachable_nodes"`
	KeyReachability map[string]bool `json:"key_reachability,omitempty"`
	ShotSamples     []ShotTruth     `json:"shot_samples"`
}

type ShotTruth struct {
	GlobalIndex int     `json:"global_index"`
	NodeID      string  `json:"node_id"`
	ShotID      string  `json:"shot_id"`
	SamplesMS   []int64 `json:"samples_ms"`
}

type Sample struct {
	GlobalIndex int    `json:"global_index"`
	ShotIndex   int    `json:"shot_index"`
	NodeID      string `json:"node_id"`
	ShotID      string `json:"shot_id"`
	LocalMS     int64  `json:"local_ms"`
	GlobalMS    int64  `json:"global_ms"`
}

type PresentedShot struct {
	GlobalIndex int      `json:"global_index"`
	NodeID      string   `json:"node_id"`
	LocalIndex  int      `json:"local_index"`
	ShotID      string   `json:"shot_id"`
	DurationMS  int64    `json:"duration_ms"`
	AspectPath  string   `json:"aspect_path"`
	Camera      string   `json:"camera,omitempty"`
	Motion      string   `json:"motion,omitempty"`
	Transition  string   `json:"transition,omitempty"`
	StartMS     int64    `json:"start_ms"`
	EndMS       int64    `json:"end_ms"`
	Samples     []Sample `json:"samples"`
	ContentHash string   `json:"content_hash"`
}

type RunResult struct {
	ProjectID        string                       `json:"project_id"`
	ContentHash      string                       `json:"content_hash"`
	AlgorithmVersion string                       `json:"algorithm_version"`
	StructureVersion string                       `json:"structure_version"`
	EventOrder       []string                     `json:"event_order"`
	ReachableNodes   []string                     `json:"reachable_nodes"`
	TerminalFlags    map[string]bool              `json:"terminal_flags"`
	Characters       map[string]map[string]string `json:"characters"`
	Shots            []PresentedShot              `json:"shots"`
	ConsumedChoices  int                          `json:"consumed_choices"`
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
