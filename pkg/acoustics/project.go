package acoustics

// LoadProjectFiles loads and validates a generated level plus truth manifest.
func LoadProjectFiles(levelPath, truthPath string) (*Level, *TruthFile, error) {
	level, err := LoadLevelFile(levelPath)
	if err != nil {
		return nil, nil, err
	}
	if err := Validate(level); err != nil {
		return nil, nil, err
	}
	truth, err := LoadTruthFile(truthPath)
	if err != nil {
		return nil, nil, err
	}
	if err := truth.MatchesListeners(level); err != nil {
		return nil, nil, err
	}
	return level, truth, nil
}
