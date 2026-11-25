package dressbatch

import (
	"crypto/sha256"
	"encoding/hex"
)

func hashValue(value any) string {
	data, err := canonicalJSON(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func PartHash(part Part) string {
	return hashValue(struct {
		ID            string    `json:"id"`
		MountID       string    `json:"mount_id"`
		VertexCount   int       `json:"vertex_count"`
		TriangleCount int       `json:"triangle_count"`
		IndexCount    int       `json:"index_count"`
		MaterialSlots []string  `json:"material_slots"`
		Submeshes     []Submesh `json:"submeshes"`
		Skin          bool      `json:"skin"`
		SkinGroup     string    `json:"skin_group"`
		Priority      int       `json:"priority"`
	}{
		ID:            part.ID,
		MountID:       part.MountID,
		VertexCount:   part.VertexCount,
		TriangleCount: part.TriangleCount,
		IndexCount:    part.EffectiveIndexCount(),
		MaterialSlots: sortedCopy(part.MaterialSlots),
		Submeshes:     sortedSubmeshes(part.Submeshes),
		Skin:          part.Skin,
		SkinGroup:     part.SkinGroup,
		Priority:      part.Priority,
	})
}

// PartFitHash covers only fields that can change batching decisions. It is used
// for parts that were considered but deferred by a batch, so a layout-only edit
// does not needlessly invalidate earlier batches.
func PartFitHash(part Part) string {
	return hashValue(struct {
		ID            string   `json:"id"`
		MountID       string   `json:"mount_id"`
		VertexCount   int      `json:"vertex_count"`
		IndexCount    int      `json:"index_count"`
		MaterialSlots []string `json:"material_slots"`
		Skin          bool     `json:"skin"`
		SkinGroup     string   `json:"skin_group"`
		Priority      int      `json:"priority"`
	}{
		ID:            part.ID,
		MountID:       part.MountID,
		VertexCount:   part.VertexCount,
		IndexCount:    part.EffectiveIndexCount(),
		MaterialSlots: sortedCopy(part.MaterialSlots),
		Skin:          part.Skin,
		SkinGroup:     part.SkinGroup,
		Priority:      part.Priority,
	})
}

func sortedSubmeshes(submeshes []Submesh) []Submesh {
	out := append([]Submesh(nil), submeshes...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].ID < out[i].ID {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func ConstraintVersion(constraints BatchConstraints) string {
	return hashValue(constraints)
}

func materialVersions(project *Project, slots []string) []MaterialSlot {
	byID := make(map[string]string, len(project.Materials))
	for _, material := range project.Materials {
		byID[material.ID] = material.Version
	}
	out := make([]MaterialSlot, 0, len(slots))
	for _, slot := range sortedUnique(slots) {
		out = append(out, MaterialSlot{ID: slot, Version: byID[slot]})
	}
	return out
}

func MaterialVersion(project *Project, slots []string) string {
	return hashValue(materialVersions(project, slots))
}

func AllMaterialVersion(project *Project) string {
	slots := make([]string, 0, len(project.Materials))
	for _, material := range project.Materials {
		slots = append(slots, material.ID)
	}
	return MaterialVersion(project, slots)
}

func ExclusionFingerprint(project *Project, partIDs []string) string {
	partSet := make(map[string]struct{}, len(partIDs))
	for _, partID := range partIDs {
		partSet[partID] = struct{}{}
	}
	rules := make([]MutualExclusion, 0)
	for _, rule := range project.MutualExclusions {
		_, a := partSet[rule.A]
		_, b := partSet[rule.B]
		if a || b {
			rules = append(rules, rule)
		}
	}
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			if rules[j].ID < rules[i].ID {
				rules[i], rules[j] = rules[j], rules[i]
			}
		}
	}
	return hashValue(rules)
}

func BatchContentHash(batch Batch) string {
	batch.ContentHash = ""
	batch.CacheKey = ""
	return hashValue(batch)
}

func BatchCacheKey(project *Project, batch Batch, upstreamKey string) string {
	parts := batchDecisionPartIDs(batch)
	partHashes := make([]struct {
		ID   string `json:"id"`
		Role string `json:"role"`
		Hash string `json:"hash"`
	}, 0, len(parts))
	byID := partMap(project)
	decisionSlots := make([]string, 0)
	members := stringSet(batch.PartIDs)
	for _, partID := range parts {
		part := byID[partID]
		role := "deferred"
		hash := PartFitHash(part)
		if _, member := members[partID]; member {
			role = "member"
			hash = PartHash(part)
		}
		partHashes = append(partHashes, struct {
			ID   string `json:"id"`
			Role string `json:"role"`
			Hash string `json:"hash"`
		}{ID: partID, Role: role, Hash: hash})
		decisionSlots = append(decisionSlots, part.MaterialSlots...)
	}
	decisionSlots = append(decisionSlots, batch.MaterialSlots...)
	key := struct {
		PartHashes           any    `json:"part_hashes"`
		DecisionPartIDs      any    `json:"decision_part_ids"`
		MaterialSlots        any    `json:"material_slots"`
		ConstraintVersion    string `json:"constraint_version"`
		ExclusionFingerprint string `json:"exclusion_fingerprint"`
		AlgorithmVersion     string `json:"algorithm_version"`
		UpstreamCacheKey     string `json:"upstream_cache_key"`
	}{
		PartHashes:           partHashes,
		DecisionPartIDs:      parts,
		MaterialSlots:        materialVersions(project, decisionSlots),
		ConstraintVersion:    ConstraintVersion(project.Constraints),
		ExclusionFingerprint: ExclusionFingerprint(project, parts),
		AlgorithmVersion:     AlgorithmVersion,
		UpstreamCacheKey:     upstreamKey,
	}
	return hashValue(key)
}

func batchDecisionPartIDs(batch Batch) []string {
	parts := make([]string, 0, len(batch.PartIDs)+len(batch.Decisions))
	parts = append(parts, batch.PartIDs...)
	for _, decision := range batch.Decisions {
		parts = append(parts, decision.PartID)
	}
	return sortedUnique(parts)
}

func decisionMaterialSlots(project *Project, batch Batch) []string {
	byID := partMap(project)
	slots := append([]string(nil), batch.MaterialSlots...)
	for _, partID := range batchDecisionPartIDs(batch) {
		if part, exists := byID[partID]; exists {
			slots = append(slots, part.MaterialSlots...)
		}
	}
	return sortedUnique(slots)
}

func ProjectContentHash(project *Project) string {
	if project == nil {
		return ""
	}
	parts := append([]Part(nil), project.Parts...)
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			if parts[j].ID < parts[i].ID {
				parts[i], parts[j] = parts[j], parts[i]
			}
		}
	}
	materials := append([]MaterialSlot(nil), project.Materials...)
	for i := 0; i < len(materials); i++ {
		for j := i + 1; j < len(materials); j++ {
			if materials[j].ID < materials[i].ID {
				materials[i], materials[j] = materials[j], materials[i]
			}
		}
	}
	rules := append([]MutualExclusion(nil), project.MutualExclusions...)
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			if rules[j].ID < rules[i].ID {
				rules[i], rules[j] = rules[j], rules[i]
			}
		}
	}
	mountIDs := make([]string, 0, len(project.Mounts))
	for _, mount := range project.Mounts {
		mountIDs = append(mountIDs, mount.ID)
	}
	mountIDs = sortedUnique(mountIDs)
	return hashValue(struct {
		ID               string            `json:"id"`
		Materials        []MaterialSlot    `json:"materials"`
		MountIDs         []string          `json:"mount_ids"`
		Parts            []Part            `json:"parts"`
		MutualExclusions []MutualExclusion `json:"mutual_exclusions"`
		Constraints      BatchConstraints  `json:"constraints"`
		AlgorithmVersion string            `json:"algorithm_version"`
	}{
		ID:               project.ID,
		Materials:        materials,
		MountIDs:         mountIDs,
		Parts:            parts,
		MutualExclusions: rules,
		Constraints:      project.Constraints,
		AlgorithmVersion: AlgorithmVersion,
	})
}

func partMap(project *Project) map[string]Part {
	out := make(map[string]Part, len(project.Parts))
	for _, part := range project.Parts {
		out[part.ID] = part
	}
	return out
}

func materialVersionMap(project *Project) map[string]string {
	out := make(map[string]string, len(project.Materials))
	for _, material := range project.Materials {
		out[material.ID] = material.Version
	}
	return out
}
