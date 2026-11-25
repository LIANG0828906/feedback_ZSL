package plancache

import (
	"fmt"
	"sort"
	"strings"
)

type KeyComponents struct {
	TemplateText  string `json:"template_text"`
	TemplateHash  string `json:"template_hash"`
	ParamTypeSeq  string `json:"param_type_seq"`
	NodeStructure string `json:"node_structure"`
	StructureHash string `json:"structure_hash"`
	Settings      string `json:"settings"`
	SettingsHash  string `json:"settings_hash"`
	Algorithm     string `json:"algorithm"`
}

type CacheKeyResult struct {
	StatementID string        `json:"statement_id"`
	CacheKey    string        `json:"cache_key"`
	Components  KeyComponents `json:"components"`
}

func deterministicSettingString(names []string, settings map[string]string) string {
	keys := append([]string(nil), names...)
	if len(keys) == 0 {
		for k := range settings {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+settings[k])
	}
	return strings.Join(parts, ";")
}

// DeriveCacheKey derives the stable key. The statement ID is deliberately not
// part of the key: identical templates/types/plans/settings must share a key.
func DeriveCacheKey(p *Project, stmt Statement, settings map[string]string) CacheKeyResult {
	effective := p.EffectiveSettings(settings)
	settingText := deterministicSettingString(p.SortedKeySettingNames(), effective)
	templateHash := hashText("template/v1", stmt.Template)
	paramSeq := strings.Join(stmt.ParamTypes(), ",")
	structure := StructureSignature(stmt)
	structureHash := hashText("structure/v1", structure)
	settingsHash := hashText("settings/v1", settingText)
	key := "ck_" + hashText(
		"plancache.key/v1",
		stmt.Template,
		paramSeq,
		structure,
		settingText,
	)[:40]
	return CacheKeyResult{
		StatementID: stmt.ID,
		CacheKey:    key,
		Components: KeyComponents{
			TemplateText:  stmt.Template,
			TemplateHash:  templateHash,
			ParamTypeSeq:  paramSeq,
			NodeStructure: structure,
			StructureHash: structureHash,
			Settings:      settingText,
			SettingsHash:  settingsHash,
			Algorithm:     "sha256-domain-separated-v1",
		},
	}
}

func DeriveAllKeys(p *Project, settings map[string]string) ([]CacheKeyResult, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	out := make([]CacheKeyResult, 0, len(p.Statements))
	for _, stmt := range p.Statements {
		out = append(out, DeriveCacheKey(p, stmt, settings))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StatementID < out[j].StatementID })
	return out, nil
}

func KeySet(results []CacheKeyResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.CacheKey)
	}
	sort.Strings(out)
	return out
}

func FormatKeyReport(results []CacheKeyResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s %s params=[%s] settings={%s}\n", r.StatementID, r.CacheKey, r.Components.ParamTypeSeq, r.Components.Settings)
	}
	return b.String()
}
