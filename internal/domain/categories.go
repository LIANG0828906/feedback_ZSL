package domain

func StandardCategories() []Category {
	return []Category{
		{Code: "SIL", Family: "silence", Label: "silence / neutral rest"},
		{Code: "MBP", Family: "closure", Label: "bilabial closure"},
		{Code: "FV", Family: "labiodental", Label: "f/v lip bite"},
		{Code: "TDN", Family: "alveolar", Label: "tongue alveolar"},
		{Code: "SZ", Family: "sibilant", Label: "sibilant teeth"},
		{Code: "KG", Family: "velar", Label: "velar stop"},
		{Code: "L", Family: "liquid", Label: "l/r liquid"},
		{Code: "A", Family: "vowel-open", Label: "open a"},
		{Code: "E", Family: "vowel-mid", Label: "mid e"},
		{Code: "I", Family: "vowel-front", Label: "front i"},
		{Code: "O", Family: "vowel-rounded", Label: "rounded o"},
		{Code: "U", Family: "vowel-rounded", Label: "tight u"},
		{Code: "TR", Family: "bridge", Label: "deterministic transition bridge"},
	}
}

// ContinuousFamily reports whether two adjacent frames are allowed without a
// bridge. Silence and the bridge category are compatible with every family.
func ContinuousFamily(left, right string) bool {
	if left == right {
		return true
	}
	if left == "silence" || right == "silence" || left == "bridge" || right == "bridge" {
		return true
	}
	adjacent := map[string]map[string]bool{
		"closure":       {"labiodental": true, "vowel-rounded": true},
		"labiodental":   {"closure": true, "sibilant": true},
		"alveolar":      {"sibilant": true, "liquid": true, "vowel-front": true},
		"sibilant":      {"alveolar": true, "vowel-front": true},
		"velar":         {"liquid": true, "vowel-mid": true, "vowel-open": true},
		"liquid":        {"alveolar": true, "velar": true, "vowel-mid": true, "vowel-open": true},
		"vowel-open":    {"velar": true, "liquid": true, "vowel-mid": true},
		"vowel-mid":     {"vowel-open": true, "vowel-front": true, "liquid": true},
		"vowel-front":   {"vowel-mid": true, "alveolar": true, "sibilant": true},
		"vowel-rounded": {"closure": true, "vowel-open": true},
	}
	if m, ok := adjacent[left]; ok && m[right] {
		return true
	}
	if m, ok := adjacent[right]; ok && m[left] {
		return true
	}
	return false
}
