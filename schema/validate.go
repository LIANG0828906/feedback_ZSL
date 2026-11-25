package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"wardrobe/domain"
)

const CodeSchema = "E_SCHEMA"

type Issue struct {
	Code     string `json:"code"`
	Path     string `json:"path"`
	ObjectID string `json:"objectId"`
	Message  string `json:"message"`
}

type ValidationError struct {
	Issues []Issue `json:"issues"`
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return CodeSchema
	}
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		parts = append(parts, fmt.Sprintf("%s %s [%s] %s", issue.Code, issue.Path, issue.ObjectID, issue.Message))
	}
	return strings.Join(parts, "; ")
}

func (e *ValidationError) HasCode(code string) bool {
	for _, issue := range e.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

type validator struct {
	issues []Issue
}

type interval struct {
	start int
	end   int
	id    string
	kind  string
}

func LoadFile(path string) (domain.Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Bundle{}, fmt.Errorf("read bundle: %w", err)
	}
	return Decode(data)
}

func Decode(data []byte) (domain.Bundle, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var bundle domain.Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return domain.Bundle{}, &ValidationError{Issues: []Issue{{
			Code:     CodeSchema,
			Path:     "$",
			ObjectID: "bundle",
			Message:  "invalid JSON or unknown field: " + err.Error(),
		}}}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return domain.Bundle{}, &ValidationError{Issues: []Issue{{
			Code:     CodeSchema,
			Path:     "$",
			ObjectID: "bundle",
			Message:  "trailing JSON value or malformed document",
		}}}
	}
	if err := Validate(&bundle); err != nil {
		return domain.Bundle{}, err
	}
	return bundle, nil
}

func Validate(bundle *domain.Bundle) error {
	v := &validator{}
	if bundle.SchemaVersion == "" {
		v.add("$.schemaVersion", "bundle", "required field is missing")
	} else if bundle.SchemaVersion != domain.SchemaVersion {
		v.add("$.schemaVersion", "bundle", "unsupported schema version "+bundle.SchemaVersion)
	}
	if bundle.GeneratorVersion == "" {
		v.add("$.generatorVersion", "bundle", "required field is missing")
	}
	if bundle.AlgorithmVersion == "" {
		v.add("$.algorithmVersion", "bundle", "required field is missing")
	}
	if len(bundle.Projects) == 0 {
		v.add("$.projects", "bundle", "at least one project is required")
	}

	projectIDs := make(map[string]int)
	for projectIndex := range bundle.Projects {
		project := &bundle.Projects[projectIndex]
		projectPath := fmt.Sprintf("$.projects[%d]", projectIndex)
		projectObject := objectID("project", project.ProjectID, projectIndex)
		if project.SchemaVersion == "" {
			v.add(projectPath+".schemaVersion", projectObject, "required field is missing")
		} else if project.SchemaVersion != domain.SchemaVersion {
			v.add(projectPath+".schemaVersion", projectObject, "unsupported project schema version "+project.SchemaVersion)
		}
		if project.ProjectID == "" {
			v.add(projectPath+".projectId", projectObject, "required field is missing")
		} else if previous, ok := projectIDs[project.ProjectID]; ok {
			v.add(projectPath+".projectId", projectObject, fmt.Sprintf("duplicate project id also used by projects[%d]", previous))
		} else {
			projectIDs[project.ProjectID] = projectIndex
		}
		if strings.TrimSpace(project.Name) == "" {
			v.add(projectPath+".name", projectObject, "required field is missing")
		}
		if project.Revision < 1 {
			v.add(projectPath+".revision", projectObject, "must be >= 1")
		}
		v.validateProjectBody(project, projectPath)
	}
	return v.result()
}

func ValidateProjectStructure(project *domain.Project) error {
	v := &validator{}
	if project == nil {
		v.add("$.projects[0]", "project", "project is nil")
		return v.result()
	}
	projectPath := "$.projects[0]"
	projectObject := objectID("project", project.ProjectID, 0)
	if project.SchemaVersion == "" {
		v.add(projectPath+".schemaVersion", projectObject, "required field is missing")
	} else if project.SchemaVersion != domain.SchemaVersion {
		v.add(projectPath+".schemaVersion", projectObject, "unsupported project schema version "+project.SchemaVersion)
	}
	if project.ProjectID == "" {
		v.add(projectPath+".projectId", projectObject, "required field is missing")
	}
	if strings.TrimSpace(project.Name) == "" {
		v.add(projectPath+".name", projectObject, "required field is missing")
	}
	if project.Revision < 1 {
		v.add(projectPath+".revision", projectObject, "must be >= 1")
	}
	mountIDs, _ := v.validateMounts(project, projectPath)
	materialIDs := v.validateMaterials(project, projectPath)
	partIDs := v.validateParts(project, projectPath, mountIDs, materialIDs)
	v.validateExclusions(project, projectPath, partIDs)
	v.validateConstraints(project, projectPath)
	return v.result()
}

func (v *validator) validateProjectBody(project *domain.Project, projectPath string) {
	mountIDs, mountModes := v.validateMounts(project, projectPath)
	materialIDs := v.validateMaterials(project, projectPath)
	partIDs := v.validateParts(project, projectPath, mountIDs, materialIDs)
	v.validateExclusions(project, projectPath, partIDs)
	v.validateConstraints(project, projectPath)
	v.validateTruth(project, projectPath, partIDs, materialIDs, mountModes)
}

func (v *validator) result() error {
	if len(v.issues) == 0 {
		return nil
	}
	sort.SliceStable(v.issues, func(i, j int) bool {
		if v.issues[i].Path != v.issues[j].Path {
			return v.issues[i].Path < v.issues[j].Path
		}
		return v.issues[i].Message < v.issues[j].Message
	})
	return &ValidationError{Issues: v.issues}
}

func (v *validator) validateMounts(project *domain.Project, projectPath string) (map[string]int, map[string]string) {
	ids := make(map[string]int)
	modes := make(map[string]string)
	if len(project.Mounts) == 0 {
		v.add(projectPath+".mounts", "project:"+project.ProjectID, "at least one mount is required")
		return ids, modes
	}
	for index, mount := range project.Mounts {
		path := fmt.Sprintf("%s.mounts[%d]", projectPath, index)
		object := objectID("mount", mount.ID, index)
		if mount.ID == "" {
			v.add(path+".id", object, "required field is missing")
		} else if previous, ok := ids[mount.ID]; ok {
			v.add(path+".id", object, fmt.Sprintf("duplicate mount id also used by mounts[%d]", previous))
		} else {
			ids[mount.ID] = index
		}
		if strings.TrimSpace(mount.Name) == "" {
			v.add(path+".name", object, "required field is missing")
		}
		switch mount.SkinMode {
		case "any", "skinned", "rigid":
			modes[mount.ID] = mount.SkinMode
		case "":
			v.add(path+".skinMode", object, "required field is missing (any|skinned|rigid)")
		default:
			v.add(path+".skinMode", object, "unsupported skinMode "+mount.SkinMode)
		}
	}
	for index, mount := range project.Mounts {
		if mount.ParentID == "" {
			continue
		}
		path := fmt.Sprintf("%s.mounts[%d].parentId", projectPath, index)
		object := objectID("mount", mount.ID, index)
		if mount.ParentID == mount.ID {
			v.add(path, object, "mount cannot parent itself")
			continue
		}
		if _, ok := ids[mount.ParentID]; !ok {
			v.add(path, object, "referenced parent mount does not exist: "+mount.ParentID)
		}
	}
	return ids, modes
}

func (v *validator) validateMaterials(project *domain.Project, projectPath string) map[string]int {
	ids := make(map[string]int)
	if len(project.Materials) == 0 {
		v.add(projectPath+".materials", "project:"+project.ProjectID, "at least one material slot is required")
		return ids
	}
	for index, material := range project.Materials {
		path := fmt.Sprintf("%s.materials[%d]", projectPath, index)
		object := objectID("material", material.ID, index)
		if material.ID == "" {
			v.add(path+".id", object, "required field is missing")
		} else if previous, ok := ids[material.ID]; ok {
			v.add(path+".id", object, fmt.Sprintf("duplicate material slot id also used by materials[%d]", previous))
		} else {
			ids[material.ID] = index
		}
		if strings.TrimSpace(material.Name) == "" {
			v.add(path+".name", object, "required field is missing")
		}
		if material.Version < 1 {
			v.add(path+".version", object, "must be >= 1")
		}
	}
	return ids
}

func (v *validator) validateParts(project *domain.Project, projectPath string, mountIDs, materialIDs map[string]int) map[string]int {
	ids := make(map[string]int)
	if len(project.Parts) == 0 {
		v.add(projectPath+".parts", "project:"+project.ProjectID, "at least one part is required")
		return ids
	}
	for index, part := range project.Parts {
		path := fmt.Sprintf("%s.parts[%d]", projectPath, index)
		object := objectID("part", part.ID, index)
		if part.ID == "" {
			v.add(path+".id", object, "required field is missing")
		} else if previous, ok := ids[part.ID]; ok {
			v.add(path+".id", object, fmt.Sprintf("duplicate part id also used by parts[%d]", previous))
		} else {
			ids[part.ID] = index
		}
		if strings.TrimSpace(part.Name) == "" {
			v.add(path+".name", object, "required field is missing")
		}
		if part.MountID == "" {
			v.add(path+".mountId", object, "required field is missing")
		} else if _, ok := mountIDs[part.MountID]; !ok {
			v.add(path+".mountId", object, "referenced mount does not exist: "+part.MountID)
		}
		v.validateMesh(path+".mesh", object, part.Mesh, materialIDs)
	}
	return ids
}

func (v *validator) validateMesh(path, object string, mesh domain.Mesh, materialIDs map[string]int) {
	if mesh.VertexCount <= 0 {
		v.add(path+".vertexCount", object, "must be > 0")
	}
	if mesh.TriangleCount <= 0 {
		v.add(path+".triangleCount", object, "must be > 0")
	}
	indexCapacity := mesh.TriangleCount * 3
	if len(mesh.Submeshes) == 0 {
		v.add(path+".submeshes", object, "at least one submesh is required")
		return
	}
	submeshIDs := make(map[string]int)
	vertexIntervals := make([]interval, 0, len(mesh.Submeshes))
	indexIntervals := make([]interval, 0, len(mesh.Submeshes))
	for index, submesh := range mesh.Submeshes {
		submeshPath := fmt.Sprintf("%s.submeshes[%d]", path, index)
		submeshObject := object + "#" + objectID("submesh", submesh.ID, index)
		if submesh.ID == "" {
			v.add(submeshPath+".id", submeshObject, "required field is missing")
		} else if previous, ok := submeshIDs[submesh.ID]; ok {
			v.add(submeshPath+".id", submeshObject, fmt.Sprintf("duplicate submesh id also used by submeshes[%d]", previous))
		} else {
			submeshIDs[submesh.ID] = index
		}
		if submesh.MaterialSlot == "" {
			v.add(submeshPath+".materialSlot", submeshObject, "required field is missing")
		} else if _, ok := materialIDs[submesh.MaterialSlot]; !ok {
			v.add(submeshPath+".materialSlot", submeshObject, "referenced material slot does not exist: "+submesh.MaterialSlot)
		}
		if submesh.VertexStart < 0 {
			v.add(submeshPath+".vertexStart", submeshObject, "must be >= 0")
		}
		if submesh.VertexCount <= 0 {
			v.add(submeshPath+".vertexCount", submeshObject, "must be > 0")
		}
		if submesh.VertexStart+submesh.VertexCount > mesh.VertexCount {
			v.add(submeshPath+".vertexCount", submeshObject, "vertex interval exceeds mesh.vertexCount")
		}
		if submesh.IndexStart < 0 {
			v.add(submeshPath+".indexStart", submeshObject, "must be >= 0")
		}
		if submesh.IndexCount <= 0 {
			v.add(submeshPath+".indexCount", submeshObject, "must be > 0")
		} else if submesh.IndexCount%3 != 0 {
			v.add(submeshPath+".indexCount", submeshObject, "must be a multiple of 3")
		}
		if submesh.IndexStart+submesh.IndexCount > indexCapacity {
			v.add(submeshPath+".indexCount", submeshObject, "index interval exceeds mesh.triangleCount*3")
		}
		vertexIntervals = append(vertexIntervals, interval{
			start: submesh.VertexStart,
			end:   submesh.VertexStart + submesh.VertexCount,
			id:    submesh.ID,
			kind:  "vertex",
		})
		indexIntervals = append(indexIntervals, interval{
			start: submesh.IndexStart,
			end:   submesh.IndexStart + submesh.IndexCount,
			id:    submesh.ID,
			kind:  "index",
		})
	}
	v.checkOverlaps(path+".submeshes", object, vertexIntervals)
	v.checkOverlaps(path+".submeshes", object, indexIntervals)
}

func (v *validator) checkOverlaps(path, object string, intervals []interval) {
	if len(intervals) < 2 {
		return
	}
	sorted := append([]interval(nil), intervals...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].id < sorted[j].id
	})
	for index := 1; index < len(sorted); index++ {
		previous := sorted[index-1]
		current := sorted[index]
		if current.start < previous.end {
			v.add(path, object, fmt.Sprintf("%s intervals overlap: %s and %s", current.kind, previous.id, current.id))
		}
	}
}

func (v *validator) validateExclusions(project *domain.Project, projectPath string, partIDs map[string]int) {
	ids := make(map[string]int)
	for index, rule := range project.ExclusionRules {
		path := fmt.Sprintf("%s.exclusionRules[%d]", projectPath, index)
		object := objectID("exclusion", rule.ID, index)
		if rule.ID == "" {
			v.add(path+".id", object, "required field is missing")
		} else if previous, ok := ids[rule.ID]; ok {
			v.add(path+".id", object, fmt.Sprintf("duplicate exclusion id also used by exclusionRules[%d]", previous))
		} else {
			ids[rule.ID] = index
		}
		if rule.PartA == "" {
			v.add(path+".partA", object, "required field is missing")
		} else if _, ok := partIDs[rule.PartA]; !ok {
			v.add(path+".partA", object, "referenced part does not exist: "+rule.PartA)
		}
		if rule.PartB == "" {
			v.add(path+".partB", object, "required field is missing")
		} else if _, ok := partIDs[rule.PartB]; !ok {
			v.add(path+".partB", object, "referenced part does not exist: "+rule.PartB)
		}
		if rule.PartA != "" && rule.PartA == rule.PartB {
			v.add(path, object, "exclusion rule must reference two different parts")
		}
		if strings.TrimSpace(rule.Description) == "" {
			v.add(path+".description", object, "required field is missing")
		}
	}
}

func (v *validator) validateConstraints(project *domain.Project, projectPath string) {
	object := "project:" + project.ProjectID
	constraints := project.Constraints
	if strings.TrimSpace(constraints.Version) == "" {
		v.add(projectPath+".constraints.version", object, "required field is missing")
	}
	if constraints.MaxMaterialSlots <= 0 {
		v.add(projectPath+".constraints.maxMaterialSlots", object, "must be > 0")
	}
	if constraints.MaxVertices <= 0 {
		v.add(projectPath+".constraints.maxVertices", object, "must be > 0")
	}
	if constraints.MaxIndices <= 0 {
		v.add(projectPath+".constraints.maxIndices", object, "must be > 0")
	}
}

func (v *validator) validateTruth(project *domain.Project, projectPath string, partIDs, materialIDs map[string]int, mountModes map[string]string) {
	object := "project:" + project.ProjectID
	if len(project.Truth.Cases) == 0 {
		v.add(projectPath+".truth.cases", object, "at least one truth case is required")
		return
	}
	caseIDs := make(map[string]int)
	partMap := project.PartMap()
	for caseIndex, truthCase := range project.Truth.Cases {
		path := fmt.Sprintf("%s.truth.cases[%d]", projectPath, caseIndex)
		caseObject := objectID("case", truthCase.CaseID, caseIndex)
		if truthCase.CaseID == "" {
			v.add(path+".caseId", caseObject, "required field is missing")
		} else if previous, ok := caseIDs[truthCase.CaseID]; ok {
			v.add(path+".caseId", caseObject, fmt.Sprintf("duplicate case id also used by cases[%d]", previous))
		} else {
			caseIDs[truthCase.CaseID] = caseIndex
		}
		if strings.TrimSpace(truthCase.Description) == "" {
			v.add(path+".description", caseObject, "required field is missing")
		}
		if len(truthCase.SelectedPartIDs) == 0 {
			v.add(path+".selectedPartIds", caseObject, "at least one selected part is required")
		}
		v.validatePartRefs(path+".selectedPartIds", caseObject, truthCase.SelectedPartIDs, partIDs)
		v.validatePartRefs(path+".acceptedPartIds", caseObject, truthCase.AcceptedPartIDs, partIDs)
		selectedSet := make(map[string]struct{}, len(truthCase.SelectedPartIDs))
		for _, id := range truthCase.SelectedPartIDs {
			selectedSet[id] = struct{}{}
		}
		for index, id := range truthCase.AcceptedPartIDs {
			if _, ok := selectedSet[id]; !ok {
				v.add(fmt.Sprintf("%s.acceptedPartIds[%d]", path, index), caseObject, "accepted part is not in selectedPartIds")
			}
		}
		for conflictIndex, conflict := range truthCase.Conflicts {
			conflictPath := fmt.Sprintf("%s.conflicts[%d]", path, conflictIndex)
			switch conflict.Category {
			case domain.ConflictMountContention, domain.ConflictExplicitExclude, domain.ConflictSkinIncompatible:
			default:
				v.add(conflictPath+".category", caseObject, "unsupported conflict category "+conflict.Category)
			}
			minimumPartIDs := 2
			if conflict.Category == domain.ConflictSkinIncompatible {
				minimumPartIDs = 1
			}
			if len(conflict.PartIDs) < minimumPartIDs {
				v.add(conflictPath+".partIds", caseObject, fmt.Sprintf("at least %d part ids are required", minimumPartIDs))
			}
			v.validatePartRefs(conflictPath+".partIds", caseObject, conflict.PartIDs, partIDs)
			if conflict.MountID != "" {
				if _, ok := mountModes[conflict.MountID]; !ok {
					v.add(conflictPath+".mountId", caseObject, "referenced mount does not exist: "+conflict.MountID)
				}
			}
			if conflict.Category == domain.ConflictMountContention && conflict.MountID == "" {
				v.add(conflictPath+".mountId", caseObject, "mount contention conflict requires mountId")
			}
			if conflict.Category == domain.ConflictSkinIncompatible && conflict.MountID == "" {
				v.add(conflictPath+".mountId", caseObject, "skin incompatibility conflict requires mountId")
			}
			if strings.TrimSpace(conflict.Message) == "" {
				v.add(conflictPath+".message", caseObject, "required field is missing")
			}
		}
		if truthCase.Valid != (len(truthCase.Conflicts) == 0) {
			v.add(path+".valid", caseObject, "valid must agree with an empty conflict list")
		}
		batchIDs := make(map[string]int)
		batchByID := make(map[string]domain.BatchResult)
		commitSequences := make(map[int]int)
		for batchIndex, batch := range truthCase.Batches {
			batchPath := fmt.Sprintf("%s.batches[%d]", path, batchIndex)
			if batch.ID == "" {
				v.add(batchPath+".id", caseObject, "required field is missing")
			} else if previous, ok := batchIDs[batch.ID]; ok {
				v.add(batchPath+".id", caseObject, fmt.Sprintf("duplicate batch id also used by batches[%d]", previous))
			} else {
				batchIDs[batch.ID] = batchIndex
				batchByID[batch.ID] = batch
			}
			if batch.Ordinal != batchIndex+1 {
				v.add(batchPath+".ordinal", caseObject, fmt.Sprintf("expected %d", batchIndex+1))
			}
			if batch.CommitSequence <= 0 {
				v.add(batchPath+".commitSequence", caseObject, "must be > 0")
			} else if previous, ok := commitSequences[batch.CommitSequence]; ok {
				v.add(batchPath+".commitSequence", caseObject, fmt.Sprintf("duplicate commit sequence also used by batches[%d]", previous))
			} else {
				commitSequences[batch.CommitSequence] = batchIndex
			}
			v.validatePartRefs(batchPath+".partIds", caseObject, batch.PartIDs, partIDs)
			for partIndex, placement := range batch.Parts {
				placementPath := fmt.Sprintf("%s.parts[%d]", batchPath, partIndex)
				part, ok := partMap[placement.PartID]
				if !ok {
					v.add(placementPath+".partId", caseObject, "referenced part does not exist: "+placement.PartID)
					continue
				}
				if placement.VertexOffset < 0 {
					v.add(placementPath+".vertexOffset", caseObject, "must be >= 0")
				}
				if placement.IndexOffset < 0 {
					v.add(placementPath+".indexOffset", caseObject, "must be >= 0")
				}
				if placement.VertexOffset+part.Mesh.VertexCount > batch.VertexUsed {
					v.add(placementPath+".vertexOffset", caseObject, "placement exceeds batch vertex usage")
				}
				if placement.IndexOffset+part.Mesh.TriangleCount*3 > batch.IndexUsed {
					v.add(placementPath+".indexOffset", caseObject, "placement exceeds batch index usage")
				}
				submeshMap := make(map[string]domain.Submesh)
				for _, submesh := range part.Mesh.Submeshes {
					submeshMap[submesh.ID] = submesh
				}
				for submeshIndex, submesh := range placement.Submeshes {
					submeshPath := fmt.Sprintf("%s.submeshes[%d]", placementPath, submeshIndex)
					expected, ok := submeshMap[submesh.SubmeshID]
					if !ok {
						v.add(submeshPath+".submeshId", caseObject, "referenced submesh does not exist: "+submesh.SubmeshID)
						continue
					}
					if _, ok := materialIDs[submesh.MaterialSlot]; !ok {
						v.add(submeshPath+".materialSlot", caseObject, "referenced material slot does not exist: "+submesh.MaterialSlot)
					}
					if submesh.VertexOffset != placement.VertexOffset+expected.VertexStart {
						v.add(submeshPath+".vertexOffset", caseObject, "must equal part vertexOffset + local vertexStart")
					}
					if submesh.IndexOffset != placement.IndexOffset+expected.IndexStart {
						v.add(submeshPath+".indexOffset", caseObject, "must equal part indexOffset + local indexStart")
					}
				}
			}
			if len(batch.Parts) != len(batch.PartIDs) {
				v.add(batchPath+".partIds", caseObject, "partIds and parts placement counts must match")
			}
			for slotIndex, slot := range batch.MaterialSlots {
				if _, ok := materialIDs[slot]; !ok {
					v.add(fmt.Sprintf("%s.materialSlots[%d]", batchPath, slotIndex), caseObject, "referenced material slot does not exist: "+slot)
				}
			}
			if batch.VertexUsed > project.Constraints.MaxVertices {
				v.add(batchPath+".vertexUsed", caseObject, "exceeds maxVertices")
			}
			if batch.IndexUsed > project.Constraints.MaxIndices {
				v.add(batchPath+".indexUsed", caseObject, "exceeds maxIndices")
			}
			if len(domain.SortedUnique(batch.MaterialSlots)) > project.Constraints.MaxMaterialSlots {
				v.add(batchPath+".materialSlots", caseObject, "exceeds maxMaterialSlots")
			}
		}
		if len(truthCase.CommitSequence) != len(truthCase.Batches) {
			v.add(path+".commitSequence", caseObject, "must contain one batch id per batch")
		}
		for index, batchID := range truthCase.CommitSequence {
			batch, ok := batchByID[batchID]
			if !ok {
				v.add(fmt.Sprintf("%s.commitSequence[%d]", path, index), caseObject, "referenced batch does not exist: "+batchID)
				continue
			}
			if batch.CommitSequence != index+1 {
				v.add(fmt.Sprintf("%s.commitSequence[%d]", path, index), caseObject,
					fmt.Sprintf("batch %s has commitSequence %d, expected %d", batchID, batch.CommitSequence, index+1))
			}
		}
	}
}

func (v *validator) validatePartRefs(path, object string, ids []string, partIDs map[string]int) {
	seen := make(map[string]int)
	for index, id := range ids {
		if id == "" {
			v.add(fmt.Sprintf("%s[%d]", path, index), object, "part id must not be empty")
			continue
		}
		if previous, ok := seen[id]; ok {
			v.add(fmt.Sprintf("%s[%d]", path, index), object, fmt.Sprintf("duplicate part id also used at index %d", previous))
		} else {
			seen[id] = index
		}
		if _, ok := partIDs[id]; !ok {
			v.add(fmt.Sprintf("%s[%d]", path, index), object, "referenced part does not exist: "+id)
		}
	}
}

func (v *validator) add(path, object, message string) {
	v.issues = append(v.issues, Issue{
		Code:     CodeSchema,
		Path:     path,
		ObjectID: object,
		Message:  message,
	})
}

func objectID(kind, id string, fallback int) string {
	if id == "" {
		return fmt.Sprintf("%s[%d]", kind, fallback)
	}
	return kind + ":" + id
}
