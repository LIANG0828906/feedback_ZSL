package dressbatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

func LoadProject(r io.Reader) (*Project, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, newError(CodeSchema, "project", "json", "cannot read project: %v", err)
	}
	if err := checkProjectJSONPresence(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var project Project
	if err := decoder.Decode(&project); err != nil {
		return nil, newError(CodeSchema, "project", "json", "cannot decode project: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, newError(CodeSchema, "project", "json", "multiple JSON values are not allowed")
		}
		return nil, newError(CodeSchema, "project", "json", "cannot finish decoding project: %v", err)
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

func LoadProjectFile(path string) (*Project, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, newError(CodeSchema, "project", "file", "cannot open %q: %v", path, err)
	}
	defer file.Close()
	return LoadProject(file)
}

func MarshalProject(project *Project) ([]byte, error) {
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return nil, newError(CodeSchema, "project", "json", "cannot encode project: %v", err)
	}
	return append(data, '\n'), nil
}

func WriteProjectFile(path string, project *Project) error {
	data, err := MarshalProject(project)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return newError(CodeSchema, "project", "file", "cannot write %q: %v", path, err)
	}
	return nil
}

func ValidateProject(project *Project) error {
	if project == nil {
		return newError(CodeSchema, "project", "document", "project is nil")
	}
	if project.SchemaVersion == "" {
		return newError(CodeSchema, "project", "schema_version", "field is required")
	}
	if project.SchemaVersion != SchemaVersion {
		return newError(CodeSchema, "project", "schema_version", "unsupported schema version %q", project.SchemaVersion)
	}
	if project.ID == "" {
		return newError(CodeSchema, "project", "id", "field is required")
	}
	if project.Name == "" {
		return newError(CodeSchema, "project", "name", "field is required")
	}
	if len(project.Materials) == 0 {
		return newError(CodeSchema, "project", "materials", "at least one material slot is required")
	}
	if len(project.Mounts) == 0 {
		return newError(CodeSchema, "project", "mounts", "at least one mount is required")
	}
	if len(project.Parts) == 0 {
		return newError(CodeSchema, "project", "parts", "at least one part is required")
	}

	materials := make(map[string]struct{}, len(project.Materials))
	for i, material := range project.Materials {
		objectID := fmt.Sprintf("material:%s", material.ID)
		if material.ID == "" {
			return newError(CodeSchema, objectID, fmt.Sprintf("materials[%d].id", i), "field is required")
		}
		if material.Version == "" {
			return newError(CodeSchema, objectID, "version", "field is required")
		}
		if _, exists := materials[material.ID]; exists {
			return newError(CodeSchema, objectID, "id", "material slot id must be unique")
		}
		materials[material.ID] = struct{}{}
	}

	mounts := make(map[string]struct{}, len(project.Mounts))
	for i, mount := range project.Mounts {
		objectID := fmt.Sprintf("mount:%s", mount.ID)
		if mount.ID == "" {
			return newError(CodeSchema, objectID, fmt.Sprintf("mounts[%d].id", i), "field is required")
		}
		if _, exists := mounts[mount.ID]; exists {
			return newError(CodeSchema, objectID, "id", "mount id must be unique")
		}
		mounts[mount.ID] = struct{}{}
	}

	parts := make(map[string]Part, len(project.Parts))
	for i := range project.Parts {
		part := project.Parts[i]
		objectID := fmt.Sprintf("part:%s", part.ID)
		if part.ID == "" {
			return newError(CodeSchema, objectID, fmt.Sprintf("parts[%d].id", i), "field is required")
		}
		if _, exists := parts[part.ID]; exists {
			return newError(CodeSchema, objectID, "id", "part id must be unique")
		}
		if part.MountID == "" {
			return newError(CodeSchema, objectID, "mount_id", "field is required")
		}
		if _, exists := mounts[part.MountID]; !exists {
			return newError(CodeSchema, objectID, "mount_id", "referenced mount %q does not exist", part.MountID)
		}
		if part.VertexCount <= 0 {
			return newError(CodeSchema, objectID, "vertex_count", "must be greater than zero")
		}
		if part.TriangleCount <= 0 {
			return newError(CodeSchema, objectID, "triangle_count", "must be greater than zero")
		}
		if part.IndexCount < 0 {
			return newError(CodeSchema, objectID, "index_count", "must not be negative")
		}
		if part.IndexCount > 0 && part.IndexCount != part.TriangleCount*3 {
			return newError(CodeSchema, objectID, "index_count", "must equal triangle_count * 3")
		}
		if part.Skin && part.SkinGroup == "" {
			return newError(CodeSchema, objectID, "skin_group", "is required for a skinned part")
		}
		if len(part.MaterialSlots) == 0 {
			return newError(CodeSchema, objectID, "material_slots", "at least one material slot is required")
		}
		partSlots := make(map[string]struct{}, len(part.MaterialSlots))
		for j, slot := range part.MaterialSlots {
			if slot == "" {
				return newError(CodeSchema, objectID, fmt.Sprintf("material_slots[%d]", j), "field is required")
			}
			if _, exists := materials[slot]; !exists {
				return newError(CodeSchema, objectID, fmt.Sprintf("material_slots[%d]", j), "referenced material slot %q does not exist", slot)
			}
			if _, duplicate := partSlots[slot]; duplicate {
				return newError(CodeSchema, objectID, "material_slots", "material slot %q is duplicated", slot)
			}
			partSlots[slot] = struct{}{}
		}
		if len(part.Submeshes) == 0 {
			return newError(CodeSchema, objectID, "submeshes", "at least one submesh is required")
		}
		if err := validateSubmeshes(part, partSlots); err != nil {
			return err
		}
		parts[part.ID] = part
	}

	exclusions := make(map[string]MutualExclusion, len(project.MutualExclusions))
	exclusionPairs := make(map[string]struct{}, len(project.MutualExclusions))
	for i, rule := range project.MutualExclusions {
		objectID := fmt.Sprintf("exclusion:%s", rule.ID)
		if rule.ID == "" {
			return newError(CodeSchema, objectID, fmt.Sprintf("mutual_exclusions[%d].id", i), "field is required")
		}
		if rule.A == "" {
			return newError(CodeSchema, objectID, "a", "field is required")
		}
		if rule.B == "" {
			return newError(CodeSchema, objectID, "b", "field is required")
		}
		if rule.A == rule.B {
			return newError(CodeSchema, objectID, "b", "a part cannot exclude itself")
		}
		if _, exists := parts[rule.A]; !exists {
			return newError(CodeSchema, objectID, "a", "referenced part %q does not exist", rule.A)
		}
		if _, exists := parts[rule.B]; !exists {
			return newError(CodeSchema, objectID, "b", "referenced part %q does not exist", rule.B)
		}
		if _, exists := exclusions[rule.ID]; exists {
			return newError(CodeSchema, objectID, "id", "exclusion rule id must be unique")
		}
		pair := orderedPair(rule.A, rule.B)
		if _, duplicate := exclusionPairs[pair]; duplicate {
			return newError(CodeSchema, objectID, "a", "part pair %s is already covered by another exclusion rule", pair)
		}
		exclusions[rule.ID] = rule
		exclusionPairs[pair] = struct{}{}
	}

	if err := validateConstraints(project.Constraints); err != nil {
		return err
	}
	if err := validateTruth(project, parts); err != nil {
		return err
	}
	return nil
}

func validateSubmeshes(part Part, partSlots map[string]struct{}) error {
	objectID := fmt.Sprintf("part:%s", part.ID)
	indexCount := part.EffectiveIndexCount()
	ids := make(map[string]struct{}, len(part.Submeshes))
	for i, submesh := range part.Submeshes {
		fieldPrefix := fmt.Sprintf("submeshes[%d]", i)
		if submesh.ID == "" {
			return newError(CodeSchema, objectID, fieldPrefix+".id", "field is required")
		}
		if _, exists := ids[submesh.ID]; exists {
			return newError(CodeSchema, objectID, "submeshes", "submesh id %q is duplicated", submesh.ID)
		}
		ids[submesh.ID] = struct{}{}
		if submesh.MaterialSlot == "" {
			return newError(CodeSchema, objectID, fieldPrefix+".material_slot", "field is required")
		}
		if _, exists := partSlots[submesh.MaterialSlot]; !exists {
			return newError(CodeSchema, objectID, fieldPrefix+".material_slot", "material slot %q is not declared by the part", submesh.MaterialSlot)
		}
		if submesh.VertexStart < 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".vertex_start", "must not be negative")
		}
		if submesh.VertexCount <= 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".vertex_count", "must be greater than zero")
		}
		if submesh.VertexStart+submesh.VertexCount > part.VertexCount {
			return newError(CodeSchema, objectID, fieldPrefix+".vertex_start", "range [%d,%d) exceeds part vertex_count %d", submesh.VertexStart, submesh.VertexStart+submesh.VertexCount, part.VertexCount)
		}
		if submesh.IndexStart < 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".index_start", "must not be negative")
		}
		if submesh.IndexCount <= 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".index_count", "must be greater than zero")
		}
		if submesh.IndexCount%3 != 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".index_count", "must be divisible by 3")
		}
		if submesh.IndexStart+submesh.IndexCount > indexCount {
			return newError(CodeSchema, objectID, fieldPrefix+".index_start", "range [%d,%d) exceeds part index_count %d", submesh.IndexStart, submesh.IndexStart+submesh.IndexCount, indexCount)
		}
	}
	for i := 0; i < len(part.Submeshes); i++ {
		for j := i + 1; j < len(part.Submeshes); j++ {
			a, b := part.Submeshes[i], part.Submeshes[j]
			if rangesOverlap(a.VertexStart, a.VertexCount, b.VertexStart, b.VertexCount) {
				return newError(CodeSchema, objectID, fmt.Sprintf("submeshes[%d].vertex_start", j), "vertex range overlaps submesh %q", a.ID)
			}
			if rangesOverlap(a.IndexStart, a.IndexCount, b.IndexStart, b.IndexCount) {
				return newError(CodeSchema, objectID, fmt.Sprintf("submeshes[%d].index_start", j), "index range overlaps submesh %q", a.ID)
			}
		}
	}
	return nil
}

func validateConstraints(constraints BatchConstraints) error {
	if constraints.MaxMaterialSlotsPerBatch <= 0 {
		return newError(CodeSchema, "constraints", "max_material_slots_per_batch", "must be greater than zero")
	}
	if constraints.MaxVerticesPerBatch <= 0 {
		return newError(CodeSchema, "constraints", "max_vertices_per_batch", "must be greater than zero")
	}
	if constraints.MaxIndicesPerBatch <= 0 {
		return newError(CodeSchema, "constraints", "max_indices_per_batch", "must be greater than zero")
	}
	if constraints.Policy == "" {
		return newError(CodeSchema, "constraints", "policy", "field is required")
	}
	if constraints.Policy != PolicyPriority {
		return newError(CodeSchema, "constraints", "policy", "unsupported policy %q", constraints.Policy)
	}
	return nil
}

func validateTruth(project *Project, parts map[string]Part) error {
	if len(project.Truth.Assemblies) == 0 {
		return newError(CodeSchema, "truth", "assemblies", "field is required and must not be empty")
	}
	seen := make(map[string]struct{}, len(project.Truth.Assemblies))
	for i, assembly := range project.Truth.Assemblies {
		objectID := fmt.Sprintf("truth:%s", assembly.ID)
		fieldPrefix := fmt.Sprintf("truth.assemblies[%d]", i)
		if assembly.ID == "" {
			return newError(CodeSchema, objectID, fieldPrefix+".id", "field is required")
		}
		if _, exists := seen[assembly.ID]; exists {
			return newError(CodeSchema, objectID, "id", "truth assembly id must be unique")
		}
		seen[assembly.ID] = struct{}{}
		if assembly.Pose == "" {
			return newError(CodeSchema, objectID, fieldPrefix+".pose", "field is required")
		}
		if len(assembly.CandidatePartIDs) == 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".candidate_part_ids", "must not be empty")
		}
		candidates := make(map[string]struct{}, len(assembly.CandidatePartIDs))
		for j, partID := range assembly.CandidatePartIDs {
			if _, exists := parts[partID]; !exists {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.candidate_part_ids[%d]", fieldPrefix, j), "referenced part %q does not exist", partID)
			}
			if _, duplicate := candidates[partID]; duplicate {
				return newError(CodeSchema, objectID, fieldPrefix+".candidate_part_ids", "part %q is duplicated", partID)
			}
			candidates[partID] = struct{}{}
		}
		for j, partID := range assembly.ExpectedAcceptedPartIDs {
			if _, exists := candidates[partID]; !exists {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_accepted_part_ids[%d]", fieldPrefix, j), "part %q is not a candidate", partID)
			}
		}
		if !equalStrings(sortedUnique(assembly.ExpectedAcceptedPartIDs), assembly.ExpectedAcceptedPartIDs) {
			return newError(CodeSchema, objectID, fieldPrefix+".expected_accepted_part_ids", "must be unique and sorted")
		}
		if assembly.ExpectedLegal && len(assembly.ExpectedConflicts) != 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".expected_conflicts", "a legal assembly cannot contain conflicts")
		}
		if !assembly.ExpectedLegal && len(assembly.ExpectedConflicts) == 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".expected_conflicts", "an illegal assembly must declare at least one conflict")
		}
		if assembly.ExpectedLegal && len(assembly.ExpectedBatches) == 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".expected_batches", "a legal assembly must declare batch truth")
		}
		if !assembly.ExpectedLegal && len(assembly.ExpectedBatches) != 0 {
			return newError(CodeSchema, objectID, fieldPrefix+".expected_batches", "an illegal assembly cannot declare packed batches")
		}
		caseBatchIDs := make([]string, 0, len(assembly.ExpectedBatches))
		seenBatchIDs := make(map[string]struct{}, len(assembly.ExpectedBatches))
		for j, batch := range assembly.ExpectedBatches {
			if batch.BatchID == "" {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].batch_id", fieldPrefix, j), "field is required")
			}
			if _, duplicate := seenBatchIDs[batch.BatchID]; duplicate {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].batch_id", fieldPrefix, j), "batch id must be unique within an assembly")
			}
			seenBatchIDs[batch.BatchID] = struct{}{}
			caseBatchIDs = append(caseBatchIDs, batch.BatchID)
			if batch.VertexUsed <= 0 || batch.IndexUsed <= 0 {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d]", fieldPrefix, j), "budget usage must be greater than zero")
			}
			if len(batch.PartIDs) == 0 {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].part_ids", fieldPrefix, j), "must not be empty")
			}
			if len(batch.Placements) != len(batch.PartIDs) {
				return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].placements", fieldPrefix, j), "must contain one placement per part")
			}
			batchParts := make(map[string]struct{}, len(batch.PartIDs))
			for k, partID := range batch.PartIDs {
				if _, exists := candidates[partID]; !exists {
					return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].part_ids[%d]", fieldPrefix, j, k), "part %q is not a candidate", partID)
				}
				if _, duplicate := batchParts[partID]; duplicate {
					return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].part_ids", fieldPrefix, j), "part %q is duplicated", partID)
				}
				batchParts[partID] = struct{}{}
			}
			for k, placement := range batch.Placements {
				if _, exists := batchParts[placement.PartID]; !exists {
					return newError(CodeSchema, objectID, fmt.Sprintf("%s.expected_batches[%d].placements[%d].part_id", fieldPrefix, j, k), "part %q is not in the batch", placement.PartID)
				}
			}
		}
		if !equalStrings(caseBatchIDs, assembly.ExpectedCommitSequence) {
			return newError(CodeSchema, objectID, fieldPrefix+".expected_commit_sequence", "must equal the expected batch ids in order")
		}
	}
	return nil
}

func rangesOverlap(aStart, aCount, bStart, bCount int) bool {
	return aStart < bStart+bCount && bStart < aStart+aCount
}

func orderedPair(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := sortedCopy(values)
	write := 0
	for _, value := range out {
		if write == 0 || out[write-1] != value {
			out[write] = value
			write++
		}
	}
	return out[:write]
}

func canonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
