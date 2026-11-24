package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

const (
	InternalSchema      = "save-interop-semantics-v1"
	DefaultRuleVersion  = "mapping-v1"
	InternalTimeScaleNS = int64(1)
)

type Coordinate struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

// ConditionalContribution records the decomposition of a total semantic value.
// It is provenance for export and conflict decisions, not a separate business
// field. Fingerprints intentionally use the totals so two branches that encode
// the same business value differently remain equivalent.
type ConditionalContribution struct {
	Role        string `json:"role"`
	Value       int64  `json:"value"`
	Physical    string `json:"physicalPath"`
	ConditionID string `json:"conditionId"`
	Source      string `json:"sourceBranch"`
}

// SemanticModel is branch-independent. A branch identifier lives on the load
// result, never on this model.
type SemanticModel struct {
	Level          int64                     `json:"level"`
	Coins          int64                     `json:"coins"`
	Experience     int64                     `json:"experience"`
	Items          []string                  `json:"items"`
	Quests         []string                  `json:"quests"`
	Coordinate     Coordinate                `json:"coordinate"`
	TimeNanos      int64                     `json:"timeNanos"`
	TimeScaleNanos int64                     `json:"timeScaleNanos"`
	Conditionals   []ConditionalContribution `json:"conditionals,omitempty"`
	Flags          map[string]bool           `json:"flags,omitempty"`
}

func (m SemanticModel) Clone() SemanticModel {
	out := m
	out.Items = append([]string(nil), m.Items...)
	out.Quests = append([]string(nil), m.Quests...)
	out.Conditionals = append([]ConditionalContribution(nil), m.Conditionals...)
	if m.Flags != nil {
		out.Flags = make(map[string]bool, len(m.Flags))
		for k, v := range m.Flags {
			out.Flags[k] = v
		}
	}
	return out
}

func (m *SemanticModel) Normalize() {
	if m.TimeScaleNanos == 0 {
		m.TimeScaleNanos = InternalTimeScaleNS
	}
	sort.Slice(m.Conditionals, func(i, j int) bool {
		a, b := m.Conditionals[i], m.Conditionals[j]
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		if a.ConditionID != b.ConditionID {
			return a.ConditionID < b.ConditionID
		}
		return a.Physical < b.Physical
	})
}

type Entry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func NewEntry(name string, value any) Entry {
	switch v := value.(type) {
	case string:
		return Entry{Name: name, Value: "s:" + v}
	case int:
		return Entry{Name: name, Value: "i:" + strconv.FormatInt(int64(v), 10)}
	case int64:
		return Entry{Name: name, Value: "i:" + strconv.FormatInt(v, 10)}
	case bool:
		return Entry{Name: name, Value: "b:" + strconv.FormatBool(v)}
	default:
		return Entry{Name: name, Value: fmt.Sprintf("v:%v", v)}
	}
}

// SemanticEntries returns a stable, branch-independent item sequence. This is
// the unit used by comparisons, fingerprints, checkpoint windows, and the
// three-path consistency check.
func SemanticEntries(m SemanticModel) []Entry {
	scale := m.TimeScaleNanos
	if scale == 0 {
		scale = InternalTimeScaleNS
	}
	entries := []Entry{
		NewEntry("level", m.Level),
		NewEntry("coins", m.Coins),
		NewEntry("experience", m.Experience),
	}
	for i, item := range m.Items {
		entries = append(entries, NewEntry(fmt.Sprintf("items[%04d]", i), item))
	}
	for i, quest := range m.Quests {
		entries = append(entries, NewEntry(fmt.Sprintf("quests[%04d]", i), quest))
	}
	entries = append(entries,
		NewEntry("coordinate.x", m.Coordinate.X),
		NewEntry("coordinate.y", m.Coordinate.Y),
		NewEntry("timeNanos", m.TimeNanos),
		NewEntry("timeScaleNanos", scale),
	)
	return entries
}

func EntryMap(entries []Entry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		out[entry.Name] = entry.Value
	}
	return out
}

// CompareSemantics reports the first semantic entry that differs.
func CompareSemantics(left, right SemanticModel, sourceBranch, targetBranch string) error {
	a, b := SemanticEntries(left), SemanticEntries(right)
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return &Error{
				Code:         CodeSemEq,
				Op:           "compare-semantics",
				Message:      "first semantic mismatch",
				Semantic:     a[i].Name,
				Left:         a[i].Value,
				Right:        b[i].Value,
				SourceBranch: sourceBranch,
				TargetBranch: targetBranch,
				Window:       -1,
			}
		}
	}
	if len(a) != len(b) {
		name := "items/quests length"
		var leftValue, rightValue any = len(a), len(b)
		if len(b) > limit {
			name = b[limit].Name
			rightValue = b[limit].Value
			leftValue = "<absent>"
		} else if len(a) > limit {
			name = a[limit].Name
			leftValue = a[limit].Value
			rightValue = "<absent>"
		}
		return &Error{
			Code:         CodeSemEq,
			Op:           "compare-semantics",
			Message:      "semantic entry count mismatch",
			Semantic:     name,
			Left:         leftValue,
			Right:        rightValue,
			SourceBranch: sourceBranch,
			TargetBranch: targetBranch,
			Window:       -1,
		}
	}
	return nil
}

type fingerprintPayload struct {
	Schema         string     `json:"schema"`
	RuleVersion    string     `json:"ruleVersion"`
	TimeScaleNanos int64      `json:"timeScaleNanos"`
	Level          int64      `json:"level"`
	Coins          int64      `json:"coins"`
	Experience     int64      `json:"experience"`
	Items          []string   `json:"items"`
	Quests         []string   `json:"quests"`
	Coordinate     Coordinate `json:"coordinate"`
	TimeNanos      int64      `json:"timeNanos"`
}

func CanonicalFingerprintPayload(m SemanticModel, ruleVersion string) ([]byte, error) {
	scale := m.TimeScaleNanos
	if scale == 0 {
		scale = InternalTimeScaleNS
	}
	p := fingerprintPayload{
		Schema:         InternalSchema,
		RuleVersion:    ruleVersion,
		TimeScaleNanos: scale,
		Level:          m.Level,
		Coins:          m.Coins,
		Experience:     m.Experience,
		Items:          append([]string(nil), m.Items...),
		Quests:         append([]string(nil), m.Quests...),
		Coordinate:     m.Coordinate,
		TimeNanos:      m.TimeNanos,
	}
	return json.Marshal(p)
}

func Fingerprint(m SemanticModel, ruleVersion string) (string, error) {
	payload, err := CanonicalFingerprintPayload(m, ruleVersion)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashString(value string) string {
	return HashBytes([]byte(value))
}

type FingerprintRecord struct {
	Entry       string
	Fingerprint string
	Canonical   string
}

// CheckFingerprintCollisions is a guard for custom hash implementations or a
// future algorithm migration. It reports equal fingerprints whose canonical
// semantic bytes differ.
func CheckFingerprintCollisions(records []FingerprintRecord) error {
	byHash := make(map[string]FingerprintRecord, len(records))
	for _, record := range records {
		if previous, ok := byHash[record.Fingerprint]; ok {
			if previous.Canonical != record.Canonical {
				return &Error{
					Code:         CodeFinger,
					Op:           "fingerprint-collision",
					Message:      "different semantic values produced the same fingerprint",
					EntryA:       previous.Entry,
					EntryB:       record.Entry,
					FingerprintA: previous.Fingerprint,
					FingerprintB: record.Fingerprint,
					Window:       -1,
				}
			}
			continue
		}
		byHash[record.Fingerprint] = record
	}
	return nil
}
