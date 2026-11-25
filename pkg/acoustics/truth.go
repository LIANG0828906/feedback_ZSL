package acoustics

import (
	"encoding/json"
	"os"
	"sort"
)

type TruthFile struct {
	SchemaVersion    string          `json:"schema_version"`
	Seed             int64           `json:"seed"`
	ParamVersion     string          `json:"param_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	Tolerances       Params          `json:"tolerances"`
	ListenerTruths   []ListenerTruth `json:"listener_truths"`
}

type ListenerTruth struct {
	ListenerID string        `json:"listener_id"`
	RoomID     string        `json:"room_id"`
	Sources    []SourceTruth `json:"sources"`
}

type SourceTruth struct {
	SourceID       string            `json:"source_id"`
	SourceRoomID   string            `json:"source_room_id"`
	Connected      bool              `json:"connected"`
	RoomChain      []string          `json:"room_chain"`
	DoorChain      []string          `json:"door_chain"`
	PathLengthM    float64           `json:"path_length_m"`
	ReachableRooms []string          `json:"reachable_rooms,omitempty"`
	ValidPathCount int               `json:"valid_path_count"`
	Reflections    []ReflectionTruth `json:"reflections"`
	ExpectedSPLDB  *float64          `json:"expected_spl_db"`
	ErrorCode      string            `json:"error_code,omitempty"`
}

type ReflectionTruth struct {
	ReflectorID       string  `json:"reflector_id"`
	ReflectionPoint   Point   `json:"reflection_point"`
	PathLengthM       float64 `json:"path_length_m"`
	IncidenceAngleDeg float64 `json:"incidence_angle_deg"`
	SPLDB             float64 `json:"spl_db"`
}

func LoadTruthFile(path string) (*TruthFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t TruthFile
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, &DomainError{Code: CodeSchema, ObjectID: "truth", Field: "json", Message: err.Error()}
	}
	return &t, t.Validate()
}

// Validate checks that the truth manifest covers exactly the level's listeners.
func (t *TruthFile) Validate() error {
	ve := &ValidationError{}
	if t == nil {
		return &DomainError{Code: CodeSchema, ObjectID: "truth", Message: "truth file is nil"}
	}
	if t.SchemaVersion == "" {
		ve.Issues = append(ve.Issues, Issue{Code: CodeSchema, ObjectID: "truth", Field: "schema_version", Message: "field is required"})
	}
	seen := map[string]bool{}
	for _, lt := range t.ListenerTruths {
		if lt.ListenerID == "" {
			ve.Issues = append(ve.Issues, Issue{Code: CodeSchema, ObjectID: "truth", Field: "listener_id", Message: "listener id is required"})
		}
		if seen[lt.ListenerID] {
			ve.Issues = append(ve.Issues, Issue{Code: CodeSchema, ObjectID: lt.ListenerID, Field: "listener_id", Message: "duplicate truth listener"})
		}
		seen[lt.ListenerID] = true
	}
	return ve.OrNil()
}

func (t *TruthFile) MatchesListeners(level *Level) error {
	want := map[string]bool{}
	for _, l := range level.Listeners {
		want[l.ID] = true
	}
	got := map[string]bool{}
	for _, lt := range t.ListenerTruths {
		got[lt.ListenerID] = true
	}
	var missing, extra []string
	for id := range want {
		if !got[id] {
			missing = append(missing, id)
		}
	}
	for id := range got {
		if !want[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) > 0 {
		return &DomainError{Code: CodeSchema, ObjectID: "truth", Field: "listener_truths", Message: "truth listener set mismatch", Details: map[string][]string{"missing": missing, "extra": extra}}
	}
	return nil
}
