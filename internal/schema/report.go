package schema

type Note struct {
	Code          string `json:"code"`
	Severity      string `json:"severity"`
	FieldPath     string `json:"field_path"`
	SourceVersion string `json:"source_version,omitempty"`
	OldValue      any    `json:"old_value,omitempty"`
	NewValue      any    `json:"new_value,omitempty"`
	DefaultValue  any    `json:"default_value,omitempty"`
	Disposition   string `json:"disposition,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type Report struct {
	Operation   string       `json:"operation"`
	Source      string       `json:"source_version,omitempty"`
	Target      string       `json:"target_version,omitempty"`
	Notes       []Note       `json:"notes"`
	Truncations []Truncation `json:"truncations"`
	LossyFields []string     `json:"lossy_fields"`
}

func (r *Report) addNote(n Note) {
	r.Notes = append(r.Notes, n)
	if n.Code == "E_LOSSY" {
		r.LossyFields = append(r.LossyFields, n.FieldPath)
	}
}

func (r *Report) addTruncation(t Truncation, lossyField string) {
	if t.Remainder != 0 {
		r.Truncations = append(r.Truncations, t)
		if lossyField != "" {
			r.LossyFields = append(r.LossyFields, lossyField)
		}
	}
}
