package budget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SemanticHash returns the source-representation-independent hash. Expression
// trees are reduced to evaluated flat coefficients before hashing, so v1/v2/v3
// of the same rule produce the same semantic hash.
func (p *Project) SemanticHash() (string, error) {
	skills := append([]Skill(nil), p.Skills...)
	groups := append([]CooldownGroup(nil), p.Groups...)
	canonical := struct {
		ID        string          `json:"id"`
		Seed      int64           `json:"seed"`
		TickMS    int64           `json:"tick_ms"`
		TickCount int             `json:"tick_count"`
		Resource  ResourceRule    `json:"resource"`
		Skills    []Skill         `json:"skills"`
		Groups    []CooldownGroup `json:"groups"`
		Intents   []Intent        `json:"intents"`
	}{
		ID: p.ID, Seed: p.Seed, TickMS: p.TickMS, TickCount: p.TickCount,
		Resource: p.Resource, Skills: skills, Groups: groups, Intents: p.Intents,
	}
	// Expression pointers and traces are intentionally not part of semantic
	// identity. Flat values are populated by the loader/generator.
	canonical.Resource.RegenExpr = nil
	for i := range canonical.Skills {
		canonical.Skills[i].CDRExpr = nil
	}
	for i := range canonical.Groups {
		members := []string{}
		for _, skill := range canonical.Skills {
			if skill.GroupID == canonical.Groups[i].ID {
				members = append(members, skill.ID)
			}
		}
		canonical.Groups[i].Members = uniqueSorted(members)
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (p *Project) ContentHashMust() string {
	hash, err := p.SemanticHash()
	if err != nil {
		panic(err)
	}
	p.ContentHash = hash
	return hash
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
