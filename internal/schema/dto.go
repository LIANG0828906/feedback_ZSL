package schema

type v1Doc struct {
	Schema       string         `json:"schema"`
	ProjectID    string         `json:"project_id"`
	Seed         int64          `json:"seed"`
	TickMS       int64          `json:"tick_ms"`
	DefaultDeny  *bool          `json:"default_deny,omitempty"`
	PolicyLimits map[string]int `json:"policy_limits,omitempty"`
	Workloads    []v1Workload   `json:"workloads"`
	Policies     []v1Policy     `json:"policies"`
	Events       []v1Event      `json:"events"`
}

type v1Workload struct {
	WorkloadID     string            `json:"workload_id"`
	Namespace      string            `json:"namespace"`
	ServiceAccount string            `json:"service_account"`
	Labels         map[string]string `json:"labels"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	Exited         bool              `json:"exited,omitempty"`
}

type v1Selector struct {
	Namespace      string            `json:"namespace,omitempty"`
	MatchLabels    map[string]string `json:"match_labels,omitempty"`
	ServiceAccount []string          `json:"service_accounts,omitempty"`
	WorkloadRefs   []string          `json:"workload_refs,omitempty"`
}

type v1Window struct {
	StartTick int64 `json:"start_tick"`
	EndTick   int64 `json:"end_tick"`
}

type v1Policy struct {
	PolicyID       string      `json:"policy_id"`
	Namespace      string      `json:"namespace"`
	SourceSelector *v1Selector `json:"source_selector,omitempty"`
	TargetPort     *int        `json:"target_port,omitempty"`
	PortRange      []int       `json:"port_range,omitempty"`
	Action         string      `json:"action,omitempty"`
	ActionCode     *int        `json:"action_code,omitempty"`
	Priority       int         `json:"priority"`
	DeclaredOrder  int         `json:"declared_order"`
	Window         v1Window    `json:"effective_window"`
	State          string      `json:"state,omitempty"`
	Revision       int         `json:"revision,omitempty"`
	Replaces       string      `json:"replaces,omitempty"`
	ReplacedBy     string      `json:"replaced_by,omitempty"`
}

type v1Attempt struct {
	AttemptID        string `json:"attempt_id"`
	AtSeq            int64  `json:"at_seq"`
	Order            int    `json:"order"`
	SourceWorkloadID string `json:"source_workload_id"`
	TargetWorkloadID string `json:"target_workload_id"`
	TargetPort       int    `json:"target_port"`
	Protocol         string `json:"protocol,omitempty"`
}

type v1Change struct {
	ChangeID          string    `json:"change_id"`
	Kind              string    `json:"kind"`
	Namespace         string    `json:"namespace,omitempty"`
	PolicyID          string    `json:"policy_id,omitempty"`
	WorkloadID        string    `json:"workload_id,omitempty"`
	EditedFields      []string  `json:"edited_fields,omitempty"`
	ReplacementRuleID string    `json:"replacement_rule_id,omitempty"`
	Before            *v1Policy `json:"before,omitempty"`
	After             *v1Policy `json:"after,omitempty"`
}

type v1Event struct {
	AtSeq      int64      `json:"at_seq"`
	Order      int        `json:"order"`
	Connection *v1Attempt `json:"connection_attempt,omitempty"`
	Change     *v1Change  `json:"policy_change,omitempty"`
}

type v2Doc struct {
	APIVersion   string         `json:"apiVersion"`
	ProjectID    string         `json:"projectId"`
	Seed         int64          `json:"seed"`
	TickMS       int64          `json:"tickMs"`
	DefaultDeny  *bool          `json:"defaultDeny,omitempty"`
	PolicyLimits map[string]int `json:"policyLimits,omitempty"`
	Workloads    []v2Workload   `json:"workloads"`
	Policies     []v2Policy     `json:"policies"`
	Timeline     []v2Event      `json:"timeline"`
}

type v2Workload struct {
	WorkloadID     string            `json:"workloadId"`
	Namespace      string            `json:"namespace"`
	ServiceAccount string            `json:"serviceAccount"`
	Labels         map[string]string `json:"labels"`
	DependsOn      []string          `json:"dependsOn,omitempty"`
	Exited         bool              `json:"exited,omitempty"`
}

type v2SelectorNode struct {
	MatchLabels     map[string]string `json:"matchLabels,omitempty"`
	ServiceAccounts []string          `json:"serviceAccounts,omitempty"`
	WorkloadRefs    []string          `json:"workloadRefs,omitempty"`
}

type v2Selector struct {
	Kind          string                    `json:"kind"`
	NamespaceTree map[string]v2SelectorNode `json:"namespaceTree"`
}

type v2PortRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type v2Window struct {
	StartMS int64 `json:"startMs"`
	EndMS   int64 `json:"endMs"`
}

type v2Policy struct {
	PolicyID        string       `json:"policyId"`
	Namespace       string       `json:"namespace"`
	SourceSelector  *v2Selector  `json:"sourceSelector,omitempty"`
	PortRange       *v2PortRange `json:"portRange,omitempty"`
	ActionName      string       `json:"actionName,omitempty"`
	ActionCode      *int         `json:"actionCode,omitempty"`
	Priority        int          `json:"priority"`
	DeclaredOrder   int          `json:"declaredOrder"`
	EffectiveWindow *v2Window    `json:"effectiveWindow,omitempty"`
	State           string       `json:"state,omitempty"`
	Revision        int          `json:"revision,omitempty"`
	Replaces        string       `json:"replaces,omitempty"`
	ReplacedBy      string       `json:"replacedBy,omitempty"`
}

type v2Attempt struct {
	AttemptID        string `json:"attemptId"`
	AtMS             int64  `json:"atMs"`
	Order            int    `json:"order"`
	SourceWorkloadID string `json:"sourceWorkloadId"`
	TargetWorkloadID string `json:"targetWorkloadId"`
	TargetPort       int    `json:"targetPort"`
	Protocol         string `json:"protocol,omitempty"`
}

type v2Change struct {
	ChangeID          string    `json:"changeId"`
	Kind              string    `json:"kind"`
	Namespace         string    `json:"namespace,omitempty"`
	PolicyID          string    `json:"policyId,omitempty"`
	WorkloadID        string    `json:"workloadId,omitempty"`
	EditedFields      []string  `json:"editedFields,omitempty"`
	ReplacementRuleID string    `json:"replacementRuleId,omitempty"`
	Before            *v2Policy `json:"before,omitempty"`
	After             *v2Policy `json:"after,omitempty"`
}

type v2Event struct {
	AtMS       int64      `json:"atMs"`
	Order      int        `json:"order"`
	Connection *v2Attempt `json:"connection,omitempty"`
	Change     *v2Change  `json:"change,omitempty"`
}

type v3Doc struct {
	APIVersion     string            `json:"apiVersion"`
	ProjectName    string            `json:"projectName"`
	FixedSeed      int64             `json:"fixedSeed"`
	TickDurationMS int64             `json:"tickDurationMs"`
	DefaultDeny    *bool             `json:"defaultDeny,omitempty"`
	Quotas         map[string]int    `json:"quotas,omitempty"`
	WorkloadSet    []v3Workload      `json:"workloadSet"`
	SelectorTable  []v3SelectorEntry `json:"selectorTable"`
	PolicyEntries  []v3Policy        `json:"policyEntries"`
	Moments        []v3Moment        `json:"moments"`
}

type v3Workload struct {
	Name               string            `json:"name"`
	Namespace          string            `json:"namespace"`
	ServiceAccountName string            `json:"serviceAccountName"`
	LabelSet           map[string]string `json:"labelSet"`
	DependencyRefs     []string          `json:"dependencyRefs,omitempty"`
	Removed            bool              `json:"removed,omitempty"`
}

type v3SelectorEntry struct {
	SelectorID      string            `json:"selectorId"`
	Namespace       string            `json:"namespace,omitempty"`
	MatchLabels     map[string]string `json:"matchLabels,omitempty"`
	ServiceAccounts []string          `json:"serviceAccounts,omitempty"`
	WorkloadRefs    []string          `json:"workloadRefs,omitempty"`
}

type v3Policy struct {
	EntryID          string  `json:"entryId"`
	NamespaceName    string  `json:"namespaceName"`
	SelectorRef      string  `json:"selectorRef"`
	PortExpression   string  `json:"portExpression"`
	Effect           string  `json:"effect,omitempty"`
	EffectCode       *int    `json:"effectCode,omitempty"`
	PriorityValue    int     `json:"priorityValue"`
	DeclarationIndex int     `json:"declarationIndex"`
	TickWindow       []int64 `json:"tickWindow"`
	LifecycleState   string  `json:"lifecycleState,omitempty"`
	Revision         int     `json:"revision,omitempty"`
	Replaces         string  `json:"replaces,omitempty"`
	ReplacedBy       string  `json:"replacedBy,omitempty"`
}

type v3Connection struct {
	ConnectionID  string `json:"connectionId"`
	AtTick        int64  `json:"atTick"`
	SequenceOrder int    `json:"sequenceOrder"`
	SourceRef     string `json:"sourceRef"`
	TargetRef     string `json:"targetRef"`
	ServicePort   int    `json:"servicePort"`
	ProtocolName  string `json:"protocolName,omitempty"`
}

type v3Edit struct {
	EditID            string    `json:"editId"`
	Kind              string    `json:"kind"`
	NamespaceName     string    `json:"namespaceName,omitempty"`
	EntryRef          string    `json:"entryRef,omitempty"`
	WorkloadRef       string    `json:"workloadRef,omitempty"`
	ChangedFields     []string  `json:"changedFields,omitempty"`
	ReplacementRuleID string    `json:"replacementRuleId,omitempty"`
	Before            *v3Policy `json:"before,omitempty"`
	After             *v3Policy `json:"after,omitempty"`
}

type v3Moment struct {
	AtTick        int64         `json:"atTick"`
	SequenceOrder int           `json:"sequenceOrder"`
	Connection    *v3Connection `json:"connection,omitempty"`
	Edit          *v3Edit       `json:"edit,omitempty"`
}
