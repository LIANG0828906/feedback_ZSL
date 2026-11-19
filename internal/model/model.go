package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
)

const (
	CodeManifest = "E_MANIFEST"
	CodePolicy   = "E_POLICY"
	CodeWait     = "E_WAIT"
	CodeCluster  = "E_CLUSTER"
	CodeGit      = "E_GIT"
	CodeBusy     = "E_BUSY"
	CodeInternal = "E_INTERNAL"
)

const (
	ConclusionConverged = "converged"
	ConclusionPartial   = "partial"
	ConclusionFailed    = "failed"
)

const (
	ResolutionConverged  = "converged"
	ResolutionUnresolved = "unresolved"
	ResolutionBlocked    = "blocked"
	ResolutionNotApplied = "not_applied"
)

type BusinessError struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	ResourceKey  string `json:"resource_key,omitempty"`
	Field        string `json:"field,omitempty"`
	SourceBranch string `json:"source_branch,omitempty"`
	Err          error  `json:"-"`
}

func (e *BusinessError) Error() string {
	parts := []string{e.Code}
	if e.ResourceKey != "" {
		parts = append(parts, "resource="+e.ResourceKey)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, ": ")
}

func (e *BusinessError) Unwrap() error { return e.Err }

func NewError(code, message string) *BusinessError {
	return &BusinessError{Code: code, Message: message}
}

func ErrorCode(err error) string {
	var be *BusinessError
	if errors.As(err, &be) {
		return be.Code
	}
	if err == nil {
		return ""
	}
	return CodeInternal
}

func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type DesiredContainer struct {
	Name  string   `json:"name"`
	Image string   `json:"image"`
	Env   []EnvVar `json:"env"`
}

type DesiredDeployment struct {
	ResourceKey string
	Cluster     string
	Namespace   string
	Name        string
	Replicas    int32
	Containers  []DesiredContainer
	Raw         map[string]any
}

type BranchPolicy struct {
	ProdReplicaCeiling int
	ReplicaCeilingSet  bool
	ForbiddenImageTags []string
	ForbiddenEnvKeys   []string
	SourceFile         string
}

type Snapshot struct {
	Cluster       string
	Branch        string
	Commit        string
	Namespaces    map[string]bool
	Deployments   []DesiredDeployment
	Policy        BranchPolicy
	PolicyPresent bool
}

type ContainerState struct {
	Name  string
	Image string
	Env   map[string]string
}

type DeploymentCondition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

type DeploymentStatus struct {
	ObservedGeneration  int64
	Replicas            int32
	UpdatedReplicas     int32
	ReadyReplicas       int32
	AvailableReplicas   int32
	UnavailableReplicas int32
	Conditions          []DeploymentCondition
}

type DeploymentState struct {
	Cluster    string
	Namespace  string
	Name       string
	Generation int64
	Replicas   int32
	Containers []ContainerState
	Status     DeploymentStatus
}

func (d DeploymentState) ResourceKey() string {
	return ResourceKey(d.Cluster, d.Namespace, d.Name)
}

func ResourceKey(cluster, namespace, name string) string {
	return cluster + "/" + namespace + "/" + name
}

type Difference struct {
	ResourceKey string `json:"resource_key"`
	Field       string `json:"field"`
	Component   string `json:"component"`
	EnvKey      string `json:"env_key"`
	Expected    any    `json:"expected"`
	Actual      any    `json:"actual"`
	Resolution  string `json:"resolution"`
	Reason      string `json:"reason"`
}

func (d Difference) SortKey() string {
	return d.ResourceKey + "\x00" + d.Field + "\x00" + d.Component + "\x00" + d.EnvKey
}

func SortDifferences(items []Difference) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].SortKey() < items[j].SortKey()
	})
}

type PolicyBlock struct {
	ResourceKey      string `json:"resource_key"`
	Field            string `json:"field"`
	Value            any    `json:"value"`
	SourceBranch     string `json:"source_branch"`
	DeclaredInBranch string `json:"declared_in_branch"`
	Reason           string `json:"reason"`
}

func (p PolicyBlock) SortKey() string {
	return p.ResourceKey + "\x00" + p.Field + "\x00" + fmt.Sprint(p.Value)
}

func SortPolicyBlocks(items []PolicyBlock) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].SortKey() < items[j].SortKey()
	})
}

type ApplyResult struct {
	ResourceKey string    `json:"resource_key"`
	Action      string    `json:"action"`
	Status      string    `json:"status"`
	ErrorCode   string    `json:"error_code"`
	Message     string    `json:"message"`
	AppliedAt   time.Time `json:"applied_at"`
}

type ClusterReport struct {
	Cluster             string        `json:"cluster"`
	Branch              string        `json:"branch"`
	GitCommit           string        `json:"git_commit"`
	Observable          bool          `json:"observable"`
	ComparisonSuccess   bool          `json:"comparison_success"`
	ExpectedCount       int           `json:"expected_count"`
	DiffCount           int           `json:"diff_count"`
	PolicyBlockCount    int           `json:"policy_block_count"`
	BlockedCount        int           `json:"blocked_count"`
	ConvergedResources  int           `json:"converged_resources"`
	Convergence         string        `json:"convergence"`
	ErrorCode           string        `json:"error_code"`
	ErrorMessage        string        `json:"error_message"`
	ErrorResourceKey    string        `json:"error_resource_key"`
	ErrorField          string        `json:"error_field"`
	ErrorSourceBranch   string        `json:"error_source_branch"`
	LastObservedRoundID string        `json:"last_observed_round_id"`
	Differences         []Difference  `json:"differences"`
	PolicyBlocks        []PolicyBlock `json:"policy_blocks"`
	ApplyResults        []ApplyResult `json:"apply_results"`
}

type RoundReport struct {
	RoundID     string          `json:"round_id"`
	StartedAt   time.Time       `json:"started_at"`
	CommittedAt time.Time       `json:"committed_at"`
	DurationMS  int64           `json:"duration_ms"`
	Clusters    []ClusterReport `json:"clusters"`
}

type ClusterStatus struct {
	RoundID             string        `json:"round_id"`
	Cluster             string        `json:"cluster"`
	Branch              string        `json:"branch"`
	GitCommit           string        `json:"git_commit"`
	CommittedAt         time.Time     `json:"committed_at"`
	Observable          bool          `json:"observable"`
	ComparisonSuccess   bool          `json:"comparison_success"`
	ExpectedCount       int           `json:"expected_count"`
	DiffCount           int           `json:"diff_count"`
	PolicyBlockCount    int           `json:"policy_block_count"`
	BlockedCount        int           `json:"blocked_count"`
	ConvergedResources  int           `json:"converged_resources"`
	Convergence         string        `json:"convergence"`
	ErrorCode           string        `json:"error_code"`
	ErrorMessage        string        `json:"error_message"`
	ErrorResourceKey    string        `json:"error_resource_key"`
	ErrorField          string        `json:"error_field"`
	ErrorSourceBranch   string        `json:"error_source_branch"`
	LastObservedRoundID string        `json:"last_observed_round_id"`
	Differences         []Difference  `json:"differences"`
	PolicyBlocks        []PolicyBlock `json:"policy_blocks"`
	ApplyResults        []ApplyResult `json:"apply_results"`
}

func (c ClusterStatus) MarshalJSON() ([]byte, error) {
	type alias ClusterStatus
	if c.Differences == nil {
		c.Differences = []Difference{}
	}
	if c.PolicyBlocks == nil {
		c.PolicyBlocks = []PolicyBlock{}
	}
	if c.ApplyResults == nil {
		c.ApplyResults = []ApplyResult{}
	}
	return json.Marshal(alias(c))
}

type PreviousObservation struct {
	RoundID            string
	Differences        []Difference
	DiffCount          int
	ConvergedResources int
}

type DesiredMap map[string]DesiredDeployment

func (m DesiredMap) Sorted() []DesiredDeployment {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]DesiredDeployment, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func RawToDeployment(raw map[string]any) (*appsv1.Deployment, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var deployment appsv1.Deployment
	if err := json.Unmarshal(data, &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}
