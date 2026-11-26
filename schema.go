package leasekit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	SchemaV1 = "lease.project/v1"
	SchemaV2 = "lease.project/v2"
	SchemaV3 = "lease.project/v3"
)

type Decimal string

func (d Decimal) milli(path string) (Milli, error) {
	return DecimalSecondsToMS(string(d))
}

func (d Decimal) MarshalJSON() ([]byte, error) {
	s := strings.TrimSpace(string(d))
	if s == "" {
		return []byte("0"), nil
	}
	if _, err := DecimalSecondsToMS(s); err != nil {
		return nil, err
	}
	return []byte(s), nil
}

func (d *Decimal) UnmarshalJSON(b []byte) error {
	s := string(bytes.TrimSpace(b))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return err
		}
		s = unquoted
	}
	*d = Decimal(s)
	return nil
}

type rawEnvelope struct {
	Schema         string `json:"schema"`
	SchemaVersion  string `json:"schemaVersion"`
	SchemaVersionP string `json:"SchemaVersion"`
}

func DetectSchema(data []byte) (string, error) {
	var env rawEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", validationError(CodeInvalidSchema, "$", "invalid JSON: %v", err)
	}
	switch {
	case env.Schema == SchemaV1:
		return SchemaV1, nil
	case env.SchemaVersion == SchemaV2:
		return SchemaV2, nil
	case env.SchemaVersionP == SchemaV3:
		return SchemaV3, nil
	default:
		return "", validationError(CodeSchemaVersionMismatch, "schema", "unsupported schema envelope %+v", env)
	}
}

func Load(data []byte) (*Project, error) {
	version, err := DetectSchema(data)
	if err != nil {
		return nil, err
	}
	switch version {
	case SchemaV1:
		return loadV1(data)
	case SchemaV2:
		return loadV2(data)
	case SchemaV3:
		return loadV3(data)
	default:
		return nil, validationError(CodeSchemaVersionMismatch, "schema", "unsupported schema %q", version)
	}
}

func unmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return validationError(CodeInvalidSchema, "$", "decode failed: %v", err)
	}
	if dec.More() {
		return validationError(CodeInvalidSchema, "$", "multiple JSON documents are not allowed")
	}
	return nil
}

// ---------- V1: snake_case, ticks, flat holder table ----------

type rawV1 struct {
	Schema        string `json:"schema"`
	ProjectID     string `json:"project_id"`
	Seed          int64  `json:"seed"`
	MSPerTick     int64  `json:"ms_per_tick"`
	Boundary      string `json:"boundary_policy"`
	FailurePolicy string `json:"failure_policy"`
	Locks         []struct {
		ID                   string `json:"lock_id"`
		Mode                 string `json:"lock_mode"`
		LeaseTicks           int64  `json:"lease_ticks"`
		RenewWindowTicks     int64  `json:"renew_window_ticks"`
		MaxRenewals          int    `json:"max_renewals"`
		MaxRenewDurationTick int64  `json:"max_renew_duration_ticks"`
		RenewAttemptBudget   int    `json:"renew_attempt_budget"`
	} `json:"locks"`
	Holders []struct {
		ID       string `json:"holder_uid"`
		LockID   string `json:"lock_id"`
		Priority int    `json:"priority"`
	} `json:"holders"`
	Ops []struct {
		Seq           int    `json:"op_no"`
		HolderUID     string `json:"holder_uid"`
		Action        string `json:"action"`
		AtTick        int64  `json:"at_tick"`
		RenewForTicks int64  `json:"renew_duration_ticks"`
		ContestRef    string `json:"contest_ref"`
	} `json:"ops"`
}

func loadV1(data []byte) (*Project, error) {
	var raw rawV1
	if err := unmarshalStrict(data, &raw); err != nil {
		return nil, err
	}
	p := &Project{
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     raw.ProjectID,
		Seed:          raw.Seed,
		Boundary:      BoundaryPolicy(raw.Boundary),
		FailurePolicy: FailurePolicy(raw.FailurePolicy),
	}
	for i, l := range raw.Locks {
		ttl, err := TicksToMS(l.LeaseTicks, raw.MSPerTick)
		if err != nil {
			return nil, withPath(err, fmt.Sprintf("locks[%d].lease_ticks", i))
		}
		window, err := TicksToMS(l.RenewWindowTicks, raw.MSPerTick)
		if err != nil {
			return nil, withPath(err, fmt.Sprintf("locks[%d].renew_window_ticks", i))
		}
		maxDur, err := TicksToMS(l.MaxRenewDurationTick, raw.MSPerTick)
		if err != nil {
			return nil, withPath(err, fmt.Sprintf("locks[%d].max_renew_duration_ticks", i))
		}
		p.Resources = append(p.Resources, Resource{
			ID: l.ID, Mode: LockMode(l.Mode), LeaseTTLMS: ttl, RenewWindowMS: window,
			MaxRenewals: l.MaxRenewals, MaxRenewDurationMS: maxDur, RenewAttemptBudget: l.RenewAttemptBudget,
		})
	}
	for _, h := range raw.Holders {
		p.Holders = append(p.Holders, Holder{ID: h.ID, ResourceID: h.LockID, Priority: h.Priority})
	}
	for _, op := range raw.Ops {
		at, err := TicksToMS(op.AtTick, raw.MSPerTick)
		if err != nil {
			return nil, withOp(withPath(err, "ops.at_tick"), op.Seq, op.HolderUID)
		}
		renewFor := Milli(0)
		if op.Action == string(OpRenew) {
			renewFor, err = TicksToMS(op.RenewForTicks, raw.MSPerTick)
			if err != nil {
				return nil, withOp(withPath(err, "ops.renew_duration_ticks"), op.Seq, op.HolderUID)
			}
		}
		p.Operations = append(p.Operations, Operation{
			Seq: op.Seq, HolderID: op.HolderUID, Kind: OpKind(op.Action), AtMS: at,
			RenewForMS: renewFor, Cohort: op.ContestRef,
		})
	}
	return p, nil
}

// ---------- V2: camelCase, milliseconds remaining-TTL window, nested tree ----------

type rawV2 struct {
	SchemaVersion string `json:"schemaVersion"`
	ProjectID     string `json:"projectId"`
	Seed          int64  `json:"seed"`
	Boundary      string `json:"boundaryPolicy"`
	FailurePolicy string `json:"failurePolicy"`
	LockTree      []struct {
		LockID          string `json:"lockId"`
		LockMode        string `json:"lockMode"`
		LeaseDurationMS Milli  `json:"leaseDurationMs"`
		RenewWindow     struct {
			Kind           string `json:"kind"`
			RemainingTTLMS Milli  `json:"remainingTtlMs"`
		} `json:"renewWindow"`
		MaxRenewals        int   `json:"maxRenewals"`
		MaxRenewDurationMS Milli `json:"maxRenewDurationMs"`
		RenewAttemptBudget int   `json:"renewAttemptBudget"`
		Holders            []struct {
			HolderID string `json:"holderId"`
			Priority int    `json:"priority"`
		} `json:"holders"`
	} `json:"lockTree"`
	Operations []struct {
		Seq              int    `json:"seq"`
		HolderID         string `json:"holderId"`
		Action           string `json:"action"`
		AtMS             Milli  `json:"atMs"`
		RenewDurationMS  Milli  `json:"renewDurationMs"`
		ArbitrationGroup string `json:"arbitrationGroup"`
	} `json:"operations"`
}

func loadV2(data []byte) (*Project, error) {
	var raw rawV2
	if err := unmarshalStrict(data, &raw); err != nil {
		return nil, err
	}
	p := &Project{
		SchemaVersion: CanonicalSchemaVersion, ProjectID: raw.ProjectID, Seed: raw.Seed,
		Boundary: BoundaryPolicy(raw.Boundary), FailurePolicy: FailurePolicy(raw.FailurePolicy),
	}
	for _, node := range raw.LockTree {
		if node.RenewWindow.Kind != "" && node.RenewWindow.Kind != "remaining_ttl" {
			return nil, validationError(CodeFieldConflict, "lockTree.renewWindow.kind", "v2 supports remaining_ttl, got %q", node.RenewWindow.Kind)
		}
		p.Resources = append(p.Resources, Resource{
			ID: node.LockID, Mode: LockMode(node.LockMode), LeaseTTLMS: node.LeaseDurationMS,
			RenewWindowMS: node.RenewWindow.RemainingTTLMS, MaxRenewals: node.MaxRenewals,
			MaxRenewDurationMS: node.MaxRenewDurationMS, RenewAttemptBudget: node.RenewAttemptBudget,
		})
		for _, h := range node.Holders {
			p.Holders = append(p.Holders, Holder{ID: h.HolderID, ResourceID: node.LockID, Priority: h.Priority})
		}
	}
	for _, op := range raw.Operations {
		p.Operations = append(p.Operations, Operation{
			Seq: op.Seq, HolderID: op.HolderID, Kind: OpKind(op.Action), AtMS: op.AtMS,
			RenewForMS: op.RenewDurationMS, Cohort: op.ArbitrationGroup,
		})
	}
	return p, nil
}

// ---------- V3: PascalCase, decimal seconds absolute window, optional reason ----------

type rawV3 struct {
	SchemaVersion string `json:"SchemaVersion"`
	ProjectID     string `json:"ProjectId"`
	Seed          int64  `json:"Seed"`
	Boundary      string `json:"BoundaryPolicy"`
	FailurePolicy string `json:"FailurePolicy"`
	ClockOrigin   string `json:"ClockOrigin"`
	LockTree      []struct {
		LockID         string  `json:"LockId"`
		LockMode       string  `json:"LockMode"`
		LeaseTTLSecond Decimal `json:"LeaseTtlSeconds"`
		RenewWindow    struct {
			Kind               string  `json:"Kind"`
			OpensAtSecond      Decimal `json:"OpensAtSecond"`
			DeadlineAtSecond   Decimal `json:"DeadlineAtSecond"`
			RemainingTTLSecond Decimal `json:"RemainingTtlSeconds"`
		} `json:"RenewWindow"`
		MaxRenewals        int     `json:"MaxRenewals"`
		MaxRenewTTLSecond  Decimal `json:"MaxRenewTtlSeconds"`
		RenewAttemptBudget int     `json:"RenewAttemptBudget"`
		Holders            []struct {
			HolderID string `json:"HolderId"`
			Priority int    `json:"Priority"`
		} `json:"Holders"`
	} `json:"LockTree"`
	Operations []struct {
		Seq                   int     `json:"Seq"`
		HolderID              string  `json:"HolderId"`
		Action                string  `json:"Action"`
		AtSecond              Decimal `json:"AtSecond"`
		RenewForSecond        Decimal `json:"RenewForSeconds"`
		ArbitrationGroup      string  `json:"ArbitrationGroup"`
		ExpectedFailureReason string  `json:"ExpectedFailureReason,omitempty"`
	} `json:"Operations"`
}

func loadV3(data []byte) (*Project, error) {
	var raw rawV3
	if err := unmarshalStrict(data, &raw); err != nil {
		return nil, err
	}
	p := &Project{
		SchemaVersion: CanonicalSchemaVersion, ProjectID: raw.ProjectID, Seed: raw.Seed,
		Boundary: BoundaryPolicy(raw.Boundary), FailurePolicy: FailurePolicy(raw.FailurePolicy),
	}
	for i, node := range raw.LockTree {
		ttl, err := node.LeaseTTLSecond.milli(fmt.Sprintf("LockTree[%d].LeaseTtlSeconds", i))
		if err != nil {
			return nil, err
		}
		maxDur, err := node.MaxRenewTTLSecond.milli(fmt.Sprintf("LockTree[%d].MaxRenewTtlSeconds", i))
		if err != nil {
			return nil, err
		}
		var window Milli
		switch node.RenewWindow.Kind {
		case "absolute_initial_window":
			openMS, err := node.RenewWindow.OpensAtSecond.milli("RenewWindow.OpensAtSecond")
			if err != nil {
				return nil, err
			}
			deadlineMS, err := node.RenewWindow.DeadlineAtSecond.milli("RenewWindow.DeadlineAtSecond")
			if err != nil {
				return nil, err
			}
			if deadlineMS < openMS {
				return nil, validationError(CodeFieldConflict, fmt.Sprintf("LockTree[%d].RenewWindow", i), "deadline %dms precedes opening %dms", deadlineMS, openMS)
			}
			window = deadlineMS - openMS
			if raw.ClockOrigin == "project_zero_initial_grant" && deadlineMS != ttl {
				return nil, validationError(CodeFieldConflict, fmt.Sprintf("LockTree[%d].RenewWindow.DeadlineAtSecond", i), "absolute initial deadline %dms must equal TTL %dms for project-zero grants", deadlineMS, ttl)
			}
		case "remaining_ttl":
			window, err = node.RenewWindow.RemainingTTLSecond.milli("RenewWindow.RemainingTtlSeconds")
			if err != nil {
				return nil, err
			}
		default:
			return nil, validationError(CodeFieldConflict, fmt.Sprintf("LockTree[%d].RenewWindow.Kind", i), "unsupported window expression %q", node.RenewWindow.Kind)
		}
		p.Resources = append(p.Resources, Resource{
			ID: node.LockID, Mode: LockMode(node.LockMode), LeaseTTLMS: ttl, RenewWindowMS: window,
			MaxRenewals: node.MaxRenewals, MaxRenewDurationMS: maxDur, RenewAttemptBudget: node.RenewAttemptBudget,
		})
		for _, h := range node.Holders {
			p.Holders = append(p.Holders, Holder{ID: h.HolderID, ResourceID: node.LockID, Priority: h.Priority})
		}
	}
	for _, op := range raw.Operations {
		at, err := op.AtSecond.milli(fmt.Sprintf("Operations[%d].AtSecond", op.Seq))
		if err != nil {
			return nil, withOp(err, op.Seq, op.HolderID)
		}
		renewFor := Milli(0)
		if op.Action == string(OpRenew) {
			renewFor, err = op.RenewForSecond.milli(fmt.Sprintf("Operations[%d].RenewForSeconds", op.Seq))
			if err != nil {
				return nil, withOp(err, op.Seq, op.HolderID)
			}
		}
		p.Operations = append(p.Operations, Operation{
			Seq: op.Seq, HolderID: op.HolderID, Kind: OpKind(op.Action), AtMS: at,
			RenewForMS: renewFor, Cohort: op.ArbitrationGroup, ExpectedReason: op.ExpectedFailureReason,
		})
	}
	return p, nil
}

func withPath(err error, path string) error {
	if biz, ok := AsError(err); ok {
		if biz.Path == "" {
			biz.Path = path
		}
		return biz
	}
	return &Error{Code: CodeInvalidSchema, Path: path, Err: err}
}

// CoreEqual compares the engineering content required by acceptance. The
// optional V3 expected-failure annotation is deliberately not part of core.
func CoreEqual(a, b *Project) bool {
	if a == nil || b == nil {
		return a == b
	}
	ca, cb := a.Clone(), b.Clone()
	ca.SchemaVersion = CanonicalSchemaVersion
	cb.SchemaVersion = CanonicalSchemaVersion
	for i := range ca.Operations {
		ca.Operations[i].ExpectedReason = ""
	}
	for i := range cb.Operations {
		cb.Operations[i].ExpectedReason = ""
	}
	ja, _ := json.Marshal(ca)
	jb, _ := json.Marshal(cb)
	return bytes.Equal(ja, jb)
}

func msToDecimalSeconds(v Milli) Decimal {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(int64(v/1000), 10)
	frac := v % 1000
	if frac != 0 {
		f := strings.TrimRight(strconv.FormatInt(int64(frac)+1000, 10)[1:], "0")
		s += "." + f
	}
	if neg {
		s = "-" + s
	}
	return Decimal(s)
}

func msToTicks(v Milli) int64 {
	return int64(v / Milli(GeneratedTickMS))
}
