package dialogue

// ConvertedDocument is an explicit version conversion with both upgrade
// defaults and downgrade loss decisions retained for audit.
type ConvertedDocument struct {
	SourceVersion string            `json:"source_version"`
	TargetVersion string            `json:"target_version"`
	Data          []byte            `json:"-"`
	Upgrades      []ConversionTrace `json:"upgrade_traces,omitempty"`
	Lossy         []ConversionTrace `json:"lossy,omitempty"`
	Truncations   []Truncation      `json:"truncations,omitempty"`
	SourceLoaded  *LoadResult       `json:"-"`
}

// ConvertVersion loads any supported document into the canonical model and
// exports it to targetVersion. The returned Data is guaranteed to be readable by
// the target version loader.
func ConvertVersion(data []byte, targetVersion string) (*ConvertedDocument, error) {
	loaded, err := LoadProject(data)
	if err != nil {
		return nil, err
	}
	exported, err := ExportProject(loaded.Project, targetVersion)
	if err != nil {
		return nil, err
	}
	converted := &ConvertedDocument{
		SourceVersion: loaded.SourceVersion, TargetVersion: targetVersion,
		Data: exported.Data, Upgrades: append([]ConversionTrace(nil), loaded.Conversions...),
		Lossy:        append([]ConversionTrace(nil), exported.Lossy...),
		Truncations:  append([]Truncation(nil), exported.Truncations...),
		SourceLoaded: loaded,
	}
	return converted, nil
}
