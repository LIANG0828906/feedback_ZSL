package schema

type TranscriptEntry struct {
	Code        string `json:"code"`
	PatchID     string `json:"patch_id,omitempty"`
	Path        string `json:"path"`
	FromVersion string `json:"from_version"`
	Original    any    `json:"original"`
	Action      string `json:"action"`
	FilledValue any    `json:"filled_value,omitempty"`
}

type Transcript struct {
	SourceVersion string            `json:"source_version"`
	Entries       []TranscriptEntry `json:"entries"`
}

type LossEntry struct {
	Code        string `json:"code"`
	PatchID     string `json:"patch_id,omitempty"`
	Path        string `json:"path"`
	Field       string `json:"field"`
	Original    any    `json:"original"`
	Disposition string `json:"disposition"`
}

type LossReport struct {
	TargetVersion string      `json:"target_version"`
	Code          string      `json:"code,omitempty"`
	Entries       []LossEntry `json:"entries"`
}

func (r *LossReport) add(entry LossEntry) {
	entry.Code = "E_LOSSY"
	r.Entries = append(r.Entries, entry)
	r.Code = "E_LOSSY"
}

func (r *LossReport) HasLoss() bool { return r != nil && len(r.Entries) > 0 }
