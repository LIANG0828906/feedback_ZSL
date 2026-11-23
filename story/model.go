package story

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	ProjectSchemaVersion = 1
	MaxShotDurationMS    = int64(24 * 60 * 60 * 1000)
	MaxShotsPerNode      = 256
	MaxAspectPriorities  = 8
)

const (
	CodeSchema   = "E_SCHEMA"
	CodeCycle    = "E_CYCLE"
	CodePara     = "E_PARA"
	CodeSave     = "E_SAVE"
	CodeDiverge  = "E_DIVERGE"
	CodeNotFound = "E_NOT_FOUND"
)

// Error is the stable machine-readable error returned by loading, validation,
// save restoration and consistency checks.
type Error struct {
	Code         string `json:"code"`
	NodeID       string `json:"node_id,omitempty"`
	Field        string `json:"field,omitempty"`
	ShotSequence *int   `json:"shot_sequence,omitempty"`
	Message      string `json:"message"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.NodeID != "" {
		b.WriteString(" node=")
		b.WriteString(e.NodeID)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.ShotSequence != nil {
		b.WriteString(" shot=")
		b.WriteString(strconv.Itoa(*e.ShotSequence))
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func NewError(code, nodeID, field, message string) *Error {
	return &Error{Code: code, NodeID: nodeID, Field: field, Message: message}
}

func DivergeError(sequence int, nodeID, field, message string) *Error {
	return &Error{
		Code:         CodeDiverge,
		NodeID:       nodeID,
		Field:        field,
		ShotSequence: &sequence,
		Message:      message,
	}
}

func AsError(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

type Project struct {
	SchemaVersion int                  `json:"schema_version"`
	ID            string               `json:"id"`
	Title         string               `json:"title"`
	Seed          int64                `json:"seed"`
	InitialFlags  map[string]any       `json:"initial_flags"`
	Characters    map[string]Character `json:"characters"`
	Nodes         []Node               `json:"nodes"`
	Choices       []Choice             `json:"choices"`
	Truth         *Truth               `json:"truth,omitempty"`
}

type Character struct {
	Name  string         `json:"name"`
	State map[string]any `json:"state"`
}

type Node struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Requires []Condition `json:"requires"`
	Mutex    []string    `json:"mutex"`
	Join     []string    `json:"join"`
	Events   []Event     `json:"events"`
	Shots    []Shot      `json:"shots"`
}

type Condition struct {
	Scope     string `json:"scope"`
	Character string `json:"character,omitempty"`
	Key       string `json:"key"`
	Op        string `json:"op"`
	Value     any    `json:"value,omitempty"`
}

type Event struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Character string `json:"character,omitempty"`
	Key       string `json:"key"`
	Value     any    `json:"value"`
}

type Shot struct {
	DurationMS     int64    `json:"duration_ms"`
	AspectPriority []string `json:"aspect_priority"`
	Description    string   `json:"description,omitempty"`
}

type Choice struct {
	AtMS   int64  `json:"at_ms"`
	Target string `json:"target"`
	Label  string `json:"label"`
}

type Truth struct {
	FinalFlags      map[string]any            `json:"final_flags"`
	FinalCharacters map[string]map[string]any `json:"final_characters,omitempty"`
	EventOrder      []string                  `json:"event_order"`
	ReachableNodes  []string                  `json:"reachable_nodes"`
	ShotCount       int                       `json:"shot_count"`
	TotalDurationMS int64                     `json:"total_duration_ms"`
}

// LoadProject strictly loads a project from a local JSON file. Required field
// presence is checked before typed decoding so missing fields can be reported
// as E_SCHEMA rather than a misleading numeric error.
func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, NewError(CodeSchema, "", "file", fmt.Sprintf("cannot read project: %v", err))
	}
	return ParseProject(data)
}

func ParseProject(data []byte) (*Project, error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, NewError(CodeSchema, "", "json", err.Error())
	}
	if err := ensureEOF(dec); err != nil {
		return nil, NewError(CodeSchema, "", "json", err.Error())
	}
	if err := validateRawProject(raw); err != nil {
		return nil, err
	}

	dec = json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var p Project
	if err := dec.Decode(&p); err != nil {
		return nil, NewError(CodeSchema, "", "json", err.Error())
	}
	normalizeProject(&p)
	if err := ValidateProject(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateRawProject(raw any) error {
	obj, ok := raw.(map[string]any)
	if !ok {
		return NewError(CodeSchema, "", "project", "project must be a JSON object")
	}
	for _, field := range []string{"schema_version", "id", "title", "seed", "initial_flags", "characters", "nodes", "choices"} {
		if _, ok := obj[field]; !ok {
			return NewError(CodeSchema, "", field, "required field is missing")
		}
	}
	nodes, ok := obj["nodes"].([]any)
	if !ok {
		return NewError(CodeSchema, "", "nodes", "must be an array")
	}
	for i, item := range nodes {
		node, ok := item.(map[string]any)
		if !ok {
			return NewError(CodeSchema, "", fmt.Sprintf("nodes[%d]", i), "must be an object")
		}
		nodeID, _ := node["id"].(string)
		for _, field := range []string{"id", "title", "requires", "mutex", "join", "events", "shots"} {
			if _, ok := node[field]; !ok {
				return NewError(CodeSchema, nodeID, fmt.Sprintf("nodes[%d].%s", i, field), "required field is missing")
			}
		}
		if shots, ok := node["shots"].([]any); ok {
			for j, shotItem := range shots {
				shot, ok := shotItem.(map[string]any)
				if !ok {
					return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.shots[%d]", nodeID, j), "must be an object")
				}
				for _, field := range []string{"duration_ms", "aspect_priority"} {
					if _, ok := shot[field]; !ok {
						return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.shots[%d].%s", nodeID, j, field), "required field is missing")
					}
				}
				if _, ok := rawFixedInteger(shot["duration_ms"]); !ok {
					return NewError(CodePara, nodeID, fmt.Sprintf("%s.shots[%d].duration_ms", nodeID, j),
						"duration must be a fixed-point integer in milliseconds")
				}
			}
		}
		requires, _ := node["requires"].([]any)
		for j, condItem := range requires {
			cond, ok := condItem.(map[string]any)
			if !ok {
				return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.requires[%d]", nodeID, j), "must be an object")
			}
			for _, field := range []string{"scope", "key", "op"} {
				if _, ok := cond[field]; !ok {
					return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.requires[%d].%s", nodeID, j, field), "required field is missing")
				}
			}
			op, _ := cond["op"].(string)
			if op != "exists" && op != "not_exists" {
				if _, ok := cond["value"]; !ok {
					return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.requires[%d].value", nodeID, j), "required field is missing")
				}
			}
			if scope, _ := cond["scope"].(string); scope == "character" {
				if _, ok := cond["character"]; !ok {
					return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.requires[%d].character", nodeID, j), "required field is missing")
				}
			}
		}
		events, _ := node["events"].([]any)
		for j, eventItem := range events {
			event, ok := eventItem.(map[string]any)
			if !ok {
				return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.events[%d]", nodeID, j), "must be an object")
			}
			for _, field := range []string{"id", "kind", "key", "value"} {
				if _, ok := event[field]; !ok {
					return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.events[%d].%s", nodeID, j, field), "required field is missing")
				}
			}
			kind, _ := event["kind"].(string)
			if kind == "set_character" || kind == "inc_character" {
				if _, ok := event["character"]; !ok {
					return NewError(CodeSchema, nodeID, fmt.Sprintf("%s.events[%d].character", nodeID, j), "required field is missing")
				}
			}
		}
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return NewError(CodeSchema, "", "choices", "must be an array")
	}
	for i, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			return NewError(CodeSchema, "", fmt.Sprintf("choices[%d]", i), "must be an object")
		}
		for _, field := range []string{"at_ms", "target", "label"} {
			if _, ok := choice[field]; !ok {
				return NewError(CodeSchema, "", fmt.Sprintf("choices[%d].%s", i, field), "required field is missing")
			}
		}
		if _, ok := rawFixedInteger(choice["at_ms"]); !ok {
			return NewError(CodePara, "", fmt.Sprintf("choices[%d].at_ms", i),
				"logical timestamp must be a fixed-point integer")
		}
	}
	return nil
}

func rawFixedInteger(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return 0, false
		}
		n, err := x.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case int64:
		return x, true
	case int:
		return int64(x), true
	default:
		return 0, false
	}
}

func normalizeProject(p *Project) {
	if p.InitialFlags == nil {
		p.InitialFlags = map[string]any{}
	}
	for k, v := range p.InitialFlags {
		p.InitialFlags[k] = NormalizeValue(v)
	}
	if p.Characters == nil {
		p.Characters = map[string]Character{}
	}
	for id, ch := range p.Characters {
		if ch.State == nil {
			ch.State = map[string]any{}
		}
		for k, v := range ch.State {
			ch.State[k] = NormalizeValue(v)
		}
		p.Characters[id] = ch
	}
	for i := range p.Nodes {
		for j := range p.Nodes[i].Requires {
			p.Nodes[i].Requires[j].Value = NormalizeValue(p.Nodes[i].Requires[j].Value)
		}
		for j := range p.Nodes[i].Events {
			p.Nodes[i].Events[j].Value = NormalizeValue(p.Nodes[i].Events[j].Value)
		}
	}
	if p.Truth != nil {
		for k, v := range p.Truth.FinalFlags {
			p.Truth.FinalFlags[k] = NormalizeValue(v)
		}
		for id, st := range p.Truth.FinalCharacters {
			for k, v := range st {
				st[k] = NormalizeValue(v)
			}
			p.Truth.FinalCharacters[id] = st
		}
	}
}

// NormalizeValue converts json.Number values to the stable scalar types used
// by the engine. Story state intentionally supports only bool, string and
// numeric scalars.
func NormalizeValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := x.Int64(); err == nil {
				return n
			}
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return s
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = NormalizeValue(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = NormalizeValue(item)
		}
		return out
	default:
		return v
	}
}

func CloneProject(p *Project) (*Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var clone Project
	if err := dec.Decode(&clone); err != nil {
		return nil, err
	}
	normalizeProject(&clone)
	return &clone, nil
}

func ContentHash(p *Project) string {
	data, err := json.Marshal(p)
	if err != nil {
		return HashJSON(map[string]any{"marshal_error": err.Error()})
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte(fmt.Sprintf("marshal_error:%v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = NormalizeValue(v)
	}
	return out
}

func CloneCharacterState(in map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(in))
	for id, state := range in {
		out[id] = CloneAnyMap(state)
	}
	return out
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func ScalarEqual(a, b any) bool {
	a = NormalizeValue(a)
	b = NormalizeValue(b)
	if an, ok := numeric(a); ok {
		if bn, ok := numeric(b); ok {
			return an == bn
		}
		return false
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
}

func numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func ValuePresent(v any) bool {
	return v != nil
}
