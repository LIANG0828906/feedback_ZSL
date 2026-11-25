package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	SchemaVersion    = "wardrobe.schema/v1"
	AlgorithmVersion = "wardrobe.batch/v1"
	GeneratorVersion = "wardrobe.generator/v1"
)

const (
	ConflictMountContention  = "mount_contention"
	ConflictExplicitExclude  = "explicit_exclusion"
	ConflictSkinIncompatible = "skin_incompatible"
)

const (
	DecisionReuse          = "reuse_existing_batch"
	DecisionSplit          = "split_new_batch"
	ReasonMaterialAffinity = "material_affinity"
	ReasonBudgetFit        = "budget_fit"
	ReasonMaterialPressure = "material_slot_pressure"
	ReasonVertexPressure   = "vertex_budget_pressure"
	ReasonIndexPressure    = "index_budget_pressure"
	ReasonSkinSeparation   = "skin_separation"
	ReasonInitialBatch     = "initial_batch"
)

// Bundle is the deterministic on-disk sample format.
type Bundle struct {
	SchemaVersion    string    `json:"schemaVersion"`
	GeneratorVersion string    `json:"generatorVersion"`
	AlgorithmVersion string    `json:"algorithmVersion"`
	Seed             int64     `json:"seed"`
	Projects         []Project `json:"projects"`
}

type Project struct {
	SchemaVersion  string           `json:"schemaVersion"`
	ProjectID      string           `json:"projectId"`
	Name           string           `json:"name"`
	Revision       int              `json:"revision"`
	Mounts         []Mount          `json:"mounts"`
	Materials      []MaterialSlot   `json:"materials"`
	Parts          []Part           `json:"parts"`
	ExclusionRules []ExclusionRule  `json:"exclusionRules"`
	Constraints    BatchConstraints `json:"constraints"`
	Truth          ProjectTruth     `json:"truth"`
}

type Mount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parentId,omitempty"`
	SkinMode string `json:"skinMode"` // any, skinned, rigid
}

type MaterialSlot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type Part struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	MountID       string `json:"mountId"`
	Mesh          Mesh   `json:"mesh"`
	Skinned       bool   `json:"skinned"`
	BatchPriority int    `json:"batchPriority"`
}

type Mesh struct {
	VertexCount   int       `json:"vertexCount"`
	TriangleCount int       `json:"triangleCount"`
	Submeshes     []Submesh `json:"submeshes"`
}

type Submesh struct {
	ID           string `json:"id"`
	MaterialSlot string `json:"materialSlot"`
	VertexStart  int    `json:"vertexStart"`
	VertexCount  int    `json:"vertexCount"`
	IndexStart   int    `json:"indexStart"`
	IndexCount   int    `json:"indexCount"`
}

type ExclusionRule struct {
	ID          string `json:"id"`
	PartA       string `json:"partA"`
	PartB       string `json:"partB"`
	Description string `json:"description"`
}

type BatchConstraints struct {
	Version          string `json:"version"`
	MaxMaterialSlots int    `json:"maxMaterialSlots"`
	MaxVertices      int    `json:"maxVertices"`
	MaxIndices       int    `json:"maxIndices"`
	AllowCrossSkin   bool   `json:"allowCrossSkin"`
}

type ProjectTruth struct {
	Cases []ExpectedCase `json:"cases"`
}

type ExpectedCase struct {
	CaseID          string        `json:"caseId"`
	Description     string        `json:"description"`
	SelectedPartIDs []string      `json:"selectedPartIds"`
	Valid           bool          `json:"valid"`
	AcceptedPartIDs []string      `json:"acceptedPartIds"`
	Conflicts       []Conflict    `json:"conflicts"`
	Batches         []BatchResult `json:"batches"`
	Decisions       []Decision    `json:"decisions"`
	CommitSequence  []string      `json:"commitSequence"`
}

type CaseResult struct {
	CaseID              string          `json:"caseId"`
	Description         string          `json:"description,omitempty"`
	SelectedPartIDs     []string        `json:"selectedPartIds"`
	Valid               bool            `json:"valid"`
	AcceptedPartIDs     []string        `json:"acceptedPartIds"`
	Conflicts           []Conflict      `json:"conflicts"`
	Batches             []BatchResult   `json:"batches"`
	Decisions           []Decision      `json:"decisions"`
	CommitSequence      []string        `json:"commitSequence"`
	InputHash           string          `json:"inputHash,omitempty"`
	ConstraintVersion   string          `json:"constraintVersion,omitempty"`
	AlgorithmVersion    string          `json:"algorithmVersion,omitempty"`
	Source              string          `json:"source,omitempty"`
	ReusedBatchIDs      []string        `json:"reusedBatchIds,omitempty"`
	InvalidatedBatchIDs []string        `json:"invalidatedBatchIds,omitempty"`
	Schedule            *ScheduleReport `json:"schedule,omitempty"`
}

type ScheduleReport struct {
	MaxQueueDepth      int                 `json:"maxQueueDepth"`
	PendingQueueBudget int                 `json:"pendingQueueBudget"`
	MaxWorkers         int                 `json:"maxWorkers"`
	PerWorkerBuffer    int                 `json:"perWorkerBuffer"`
	TotalTaskWeight    int                 `json:"totalTaskWeight"`
	WaitEvents         []ScheduleWaitEvent `json:"waitEvents"`
}

type ScheduleWaitEvent struct {
	TaskID           string `json:"taskId"`
	Stage            string `json:"stage"`
	Reason           string `json:"reason"`
	WaitMilliseconds int64  `json:"waitMilliseconds"`
}

type Conflict struct {
	Category string   `json:"category"`
	MountID  string   `json:"mountId,omitempty"`
	PartIDs  []string `json:"partIds"`
	RuleID   string   `json:"ruleId,omitempty"`
	Message  string   `json:"message"`
}

type Decision struct {
	Sequence               int      `json:"sequence"`
	PartID                 string   `json:"partId"`
	BatchID                string   `json:"batchId"`
	BatchOrdinal           int      `json:"batchOrdinal"`
	ConsideredBatchID      string   `json:"consideredBatchId,omitempty"`
	ConsideredBatchOrdinal int      `json:"consideredBatchOrdinal,omitempty"`
	Result                 string   `json:"result"`
	Reason                 string   `json:"reason"`
	BlockedBy              []string `json:"blockedBy,omitempty"`
	Detail                 string   `json:"detail"`
}

type BatchResult struct {
	ID             string          `json:"id"`
	Ordinal        int             `json:"ordinal"`
	CommitSequence int             `json:"commitSequence"`
	PartIDs        []string        `json:"partIds"`
	Parts          []PartPlacement `json:"parts"`
	MaterialSlots  []string        `json:"materialSlots"`
	VertexUsed     int             `json:"vertexUsed"`
	IndexUsed      int             `json:"indexUsed"`
	Dependencies   []string        `json:"dependencies"`
	CacheKey       string          `json:"cacheKey,omitempty"`
	Source         string          `json:"source,omitempty"`
}

type PartPlacement struct {
	PartID       string             `json:"partId"`
	VertexOffset int                `json:"vertexOffset"`
	IndexOffset  int                `json:"indexOffset"`
	Submeshes    []SubmeshPlacement `json:"submeshes"`
}

type SubmeshPlacement struct {
	SubmeshID    string `json:"submeshId"`
	MaterialSlot string `json:"materialSlot"`
	VertexStart  int    `json:"vertexStart"`
	VertexCount  int    `json:"vertexCount"`
	IndexStart   int    `json:"indexStart"`
	IndexCount   int    `json:"indexCount"`
	VertexOffset int    `json:"vertexOffset"`
	IndexOffset  int    `json:"indexOffset"`
}

func (p *Project) PartMap() map[string]Part {
	out := make(map[string]Part, len(p.Parts))
	for _, part := range p.Parts {
		out[part.ID] = part
	}
	return out
}

func (p *Project) MountMap() map[string]Mount {
	out := make(map[string]Mount, len(p.Mounts))
	for _, mount := range p.Mounts {
		out[mount.ID] = mount
	}
	return out
}

func (p *Project) MaterialMap() map[string]MaterialSlot {
	out := make(map[string]MaterialSlot, len(p.Materials))
	for _, material := range p.Materials {
		out[material.ID] = material
	}
	return out
}

func (p *Project) PartByID(id string) (Part, bool) {
	for _, part := range p.Parts {
		if part.ID == id {
			return part, true
		}
	}
	return Part{}, false
}

func (p *Project) SortedPartIDs(ids []string) []string {
	partMap := p.PartMap()
	out := append([]string(nil), ids...)
	sort.Slice(out, func(i, j int) bool {
		left, lok := partMap[out[i]]
		right, rok := partMap[out[j]]
		if !lok || !rok {
			return out[i] < out[j]
		}
		return PartOrderLess(left, right)
	})
	return out
}

func PartOrderLess(left, right Part) bool {
	if left.BatchPriority != right.BatchPriority {
		return left.BatchPriority > right.BatchPriority
	}
	return left.ID < right.ID
}

func NormalizeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func SortedUnique(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func PartContentHash(part Part) string {
	return HashJSON(part)
}

func ConstraintParamsHash(constraints BatchConstraints) string {
	return HashJSON(constraints)
}

func HashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("domain: marshal hash input: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func BatchCacheKey(partHashes []string, materialVersions map[string]int, constraints BatchConstraints, upstreamKey string) string {
	hashes := append([]string(nil), partHashes...)
	sort.Strings(hashes)
	versions := make([]string, 0, len(materialVersions))
	for id, version := range materialVersions {
		versions = append(versions, fmt.Sprintf("%s@%d", id, version))
	}
	sort.Strings(versions)
	payload := struct {
		PartHashes        []string `json:"partHashes"`
		MaterialVersions  []string `json:"materialVersions"`
		ConstraintVersion string   `json:"constraintVersion"`
		ConstraintParams  string   `json:"constraintParams"`
		AlgorithmVersion  string   `json:"algorithmVersion"`
		UpstreamCacheKey  string   `json:"upstreamCacheKey"`
	}{
		PartHashes:        hashes,
		MaterialVersions:  versions,
		ConstraintVersion: constraints.Version,
		ConstraintParams:  ConstraintParamsHash(constraints),
		AlgorithmVersion:  AlgorithmVersion,
		UpstreamCacheKey:  upstreamKey,
	}
	return HashJSON(payload)
}

func CaseInputHash(project Project, selectedPartIDs []string) string {
	partMap := project.PartMap()
	mountMap := project.MountMap()
	materialMap := project.MaterialMap()
	selected := append([]string(nil), selectedPartIDs...)
	sort.Strings(selected)

	partHashes := make([]string, 0, len(selected))
	mountModes := make([]string, 0, len(selected))
	materialVersions := make([]string, 0, len(selected))
	for _, id := range selected {
		part, ok := partMap[id]
		if !ok {
			partHashes = append(partHashes, "missing:"+id)
			continue
		}
		partHashes = append(partHashes, id+"="+PartContentHash(part))
		mount := mountMap[part.MountID]
		mountModes = append(mountModes, part.MountID+"="+mount.SkinMode)
		for _, submesh := range part.Mesh.Submeshes {
			material := materialMap[submesh.MaterialSlot]
			materialVersions = append(materialVersions, submesh.MaterialSlot+"@"+fmt.Sprint(material.Version))
		}
	}
	sort.Strings(partHashes)
	sort.Strings(mountModes)
	sort.Strings(materialVersions)

	rules := make([]ExclusionRule, 0)
	for _, rule := range project.ExclusionRules {
		if contains(selected, rule.PartA) && contains(selected, rule.PartB) {
			rules = append(rules, rule)
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	payload := struct {
		ProjectID        string           `json:"projectId"`
		Selected         []string         `json:"selected"`
		PartHashes       []string         `json:"partHashes"`
		MountModes       []string         `json:"mountModes"`
		MaterialVersions []string         `json:"materialVersions"`
		Rules            []ExclusionRule  `json:"rules"`
		Constraints      BatchConstraints `json:"constraints"`
		AlgorithmVersion string           `json:"algorithmVersion"`
	}{
		ProjectID:        project.ProjectID,
		Selected:         selected,
		PartHashes:       partHashes,
		MountModes:       mountModes,
		MaterialVersions: materialVersions,
		Rules:            rules,
		Constraints:      project.Constraints,
		AlgorithmVersion: AlgorithmVersion,
	}
	return HashJSON(payload)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func JoinFieldPath(base, field string) string {
	if base == "" {
		return field
	}
	if strings.HasPrefix(field, "[") {
		return base + field
	}
	return base + "." + field
}

func ToAbortMessage(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, ": ")
}
