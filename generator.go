package leasekit

import "fmt"

const FixedSeed int64 = 20261127

const GeneratedTickMS int64 = 100

func GenerateProject(seed int64) *Project {
	if seed == 0 {
		seed = FixedSeed
	}
	p := &Project{
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     fmt.Sprintf("lease-project-seed-%d", seed),
		Seed:          seed,
		Boundary:      BoundaryLowerInclusiveUpperExclusive,
		FailurePolicy: FailureRetainDeadline,
		Resources: []Resource{
			{ID: "lock-order", Mode: ModeExclusive, LeaseTTLMS: 3000, RenewWindowMS: 1000, MaxRenewals: 2, MaxRenewDurationMS: 3000, RenewAttemptBudget: 8},
			{ID: "lock-priority", Mode: ModeShared, LeaseTTLMS: 3000, RenewWindowMS: 1000, MaxRenewals: 2, MaxRenewDurationMS: 3000, RenewAttemptBudget: 8},
			{ID: "lock-tie", Mode: ModeShared, LeaseTTLMS: 3000, RenewWindowMS: 1000, MaxRenewals: 2, MaxRenewDurationMS: 3000, RenewAttemptBudget: 8},
			{ID: "lock-abandon", Mode: ModeExclusive, LeaseTTLMS: 2000, RenewWindowMS: 500, MaxRenewals: 1, MaxRenewDurationMS: 2000, RenewAttemptBudget: 8},
			{ID: "lock-late", Mode: ModeExclusive, LeaseTTLMS: 2000, RenewWindowMS: 500, MaxRenewals: 1, MaxRenewDurationMS: 2000, RenewAttemptBudget: 8},
		},
		Holders: []Holder{
			{ID: "holder-order-a", ResourceID: "lock-order", Priority: 10},
			{ID: "holder-order-wait", ResourceID: "lock-order", Priority: 1},
			{ID: "holder-prio-high", ResourceID: "lock-priority", Priority: 10},
			{ID: "holder-prio-low", ResourceID: "lock-priority", Priority: 5},
			{ID: "holder-tie-a", ResourceID: "lock-tie", Priority: 7},
			{ID: "holder-tie-b", ResourceID: "lock-tie", Priority: 7},
			{ID: "holder-abandon", ResourceID: "lock-abandon", Priority: 1},
			{ID: "holder-after-abandon", ResourceID: "lock-abandon", Priority: 1},
			{ID: "holder-late", ResourceID: "lock-late", Priority: 1},
			{ID: "holder-late-next", ResourceID: "lock-late", Priority: 1},
		},
		Operations: []Operation{
			{Seq: 1, HolderID: "holder-order-a", Kind: OpAcquire, AtMS: 0},
			{Seq: 2, HolderID: "holder-prio-high", Kind: OpAcquire, AtMS: 0},
			{Seq: 3, HolderID: "holder-prio-low", Kind: OpAcquire, AtMS: 0},
			{Seq: 4, HolderID: "holder-tie-a", Kind: OpAcquire, AtMS: 0},
			{Seq: 5, HolderID: "holder-tie-b", Kind: OpAcquire, AtMS: 0},
			{Seq: 6, HolderID: "holder-abandon", Kind: OpAcquire, AtMS: 0},
			{Seq: 7, HolderID: "holder-late", Kind: OpAcquire, AtMS: 0},

			{Seq: 8, HolderID: "holder-abandon", Kind: OpAbandon, AtMS: 800},
			{Seq: 9, HolderID: "holder-order-wait", Kind: OpAcquire, AtMS: 1000, ExpectedReason: CodeResourceHeld},
			{Seq: 10, HolderID: "holder-order-a", Kind: OpRenew, AtMS: 1500, RenewForMS: 3000, ExpectedReason: CodeRenewTooEarly},
			{Seq: 11, HolderID: "holder-after-abandon", Kind: OpAcquire, AtMS: 1900, ExpectedReason: CodeResourceHeld},

			{Seq: 12, HolderID: "holder-order-a", Kind: OpRenew, AtMS: 2000, RenewForMS: 3000},
			{Seq: 13, HolderID: "holder-tie-a", Kind: OpRenew, AtMS: 2000, RenewForMS: 3000, Cohort: "cohort-tie"},
			{Seq: 14, HolderID: "holder-prio-low", Kind: OpRenew, AtMS: 2000, RenewForMS: 3000, Cohort: "cohort-priority", ExpectedReason: CodeCompetitionLost},
			{Seq: 15, HolderID: "holder-after-abandon", Kind: OpAcquire, AtMS: 2000},

			{Seq: 16, HolderID: "holder-tie-b", Kind: OpRenew, AtMS: 2100, RenewForMS: 3000, Cohort: "cohort-tie", ExpectedReason: CodeCompetitionLost},
			{Seq: 17, HolderID: "holder-prio-high", Kind: OpRenew, AtMS: 2100, RenewForMS: 3000, Cohort: "cohort-priority"},
			{Seq: 18, HolderID: "holder-late", Kind: OpRenew, AtMS: 2100, RenewForMS: 2000, ExpectedReason: CodeRenewExpired},
			{Seq: 19, HolderID: "holder-late-next", Kind: OpAcquire, AtMS: 2100},

			{Seq: 20, HolderID: "holder-late-next", Kind: OpRelease, AtMS: 2200},
			{Seq: 21, HolderID: "holder-tie-a", Kind: OpRelease, AtMS: 2500},

			{Seq: 22, HolderID: "holder-order-wait", Kind: OpAcquire, AtMS: 3500, ExpectedReason: CodeResourceHeld},
			{Seq: 23, HolderID: "holder-prio-high", Kind: OpRelease, AtMS: 3500},

			{Seq: 24, HolderID: "holder-after-abandon", Kind: OpRelease, AtMS: 3600},
			{Seq: 25, HolderID: "holder-order-a", Kind: OpRenew, AtMS: 4000, RenewForMS: 3000},

			{Seq: 26, HolderID: "holder-order-a", Kind: OpRenew, AtMS: 6500, RenewForMS: 3000, ExpectedReason: CodeRenewLimitReached},
			{Seq: 27, HolderID: "holder-order-wait", Kind: OpAcquire, AtMS: 6600, ExpectedReason: CodeResourceHeld},
			{Seq: 28, HolderID: "holder-order-wait", Kind: OpAcquire, AtMS: 7100},
			{Seq: 29, HolderID: "holder-order-wait", Kind: OpRelease, AtMS: 7500},
		},
	}
	return p
}

type Truth struct {
	SchemaVersion string  `json:"schemaVersion"`
	ProjectID     string  `json:"projectId"`
	Seed          int64   `json:"seed"`
	ToleranceMS   Milli   `json:"toleranceMs"`
	Report        *Report `json:"report"`
}

func GenerateTruth(p *Project) (*Truth, error) {
	report, err := Run(p)
	if err != nil {
		return nil, err
	}
	return &Truth{
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     p.ProjectID,
		Seed:          p.Seed,
		ToleranceMS:   0,
		Report:        report,
	}, nil
}
