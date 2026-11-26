package locklab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	MaxWorkers              = 64
	MinWorkerMemoryBytes    = int64(4096)
	DefaultWorkerBudget     = 4
	DefaultMemoryBudgetByte = int64(1 << 20)
)

// LoadProject decodes one of the three supported schema versions into the
// normalized project model. The input is never modified.
func LoadProject(data []byte) (*Project, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, NewError(EJSONInvalid, "$", "invalid JSON object: %v", err)
	}
	versionKey := ""
	for _, candidate := range []string{"schema_version", "schema", "SchemaVersion"} {
		if _, ok := top[candidate]; ok {
			if versionKey != "" {
				return nil, NewError(ESchemaAmbiguous, "$",
					"multiple schema version fields: %s and %s", versionKey, candidate)
			}
			versionKey = candidate
		}
	}
	if versionKey == "" {
		return nil, NewError(ESchemaVersionMissing, "$", "schema version field is required")
	}

	switch versionKey {
	case "schema_version":
		return loadV1(data)
	case "schema":
		return loadV2(data)
	case "SchemaVersion":
		return loadV3(data)
	default:
		return nil, NewError(ESchemaUnsupported, "$", "unsupported schema discriminator %q", versionKey)
	}
}

func LoadProjectFile(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, WrapError(EJSONInvalid, "$", err, "cannot read project %s", path)
	}
	return LoadProject(data)
}

func rejectDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := scanJSONValue(dec, "$"); err != nil {
		if IsErrorCode(err, EJSONDuplicateKey) {
			return err
		}
		return NewError(EJSONInvalid, "$", "invalid JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return NewError(EJSONInvalid, "$", "trailing JSON value")
		}
		return NewError(EJSONInvalid, "$", "invalid trailing JSON: %v", err)
	}
	return nil
}

func scanJSONValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return NewError(EJSONInvalid, path, "object key is not a string")
			}
			childPath := jsonPathField(path, key)
			if _, exists := seen[key]; exists {
				return NewError(EJSONDuplicateKey, childPath, "duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(dec, childPath); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		index := 0
		for dec.More() {
			if err := scanJSONValue(dec, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		_, err = dec.Token()
		return err
	default:
		return NewError(EJSONInvalid, path, "unexpected JSON delimiter %q", delim)
	}
}

func jsonPathField(parent, field string) string {
	if isSimpleJSONPathField(field) {
		return parent + "." + field
	}
	return parent + "[" + strconv.Quote(field) + "]"
}

func isSimpleJSONPathField(field string) bool {
	if field == "" {
		return false
	}
	for i, r := range field {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' ||
			(i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func decodeStrict(data []byte, dst any, path string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return NewError(ESchemaUnknownField, path, "%v", err)
		}
		return NewError(EJSONInvalid, path, "cannot decode schema: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return NewError(EJSONInvalid, path, "trailing JSON data")
	}
	return nil
}

type v1LockMode struct {
	Code           *string  `json:"code"`
	Mode           *string  `json:"mode"`
	CompatibleWith []string `json:"compatible_with"`
}

type v1Item struct {
	ItemID       *string `json:"item_id"`
	TableName    *string `json:"table_name"`
	InitialValue string  `json:"initial_value"`
}

type v1Operation struct {
	LocalSeq       *int    `json:"local_seq"`
	AtMs           *int64  `json:"at_ms"`
	Kind           *string `json:"kind"`
	ItemID         string  `json:"item_id,omitempty"`
	TableName      string  `json:"table_name,omitempty"`
	Granularity    string  `json:"granularity,omitempty"`
	LockMode       string  `json:"lock_mode,omitempty"`
	Value          string  `json:"value,omitempty"`
	WaitTimeoutMs  *int64  `json:"wait_timeout_ms,omitempty"`
	WaitDeadlineMs *int64  `json:"wait_deadline_ms,omitempty"`
	NoWait         bool    `json:"no_wait,omitempty"`
}

type v1Transaction struct {
	TransactionID *string       `json:"transaction_id"`
	Priority      *int          `json:"priority"`
	Isolation     *string       `json:"isolation_level"`
	Operations    []v1Operation `json:"operations"`
}

type v1Document struct {
	SchemaVersion     *int            `json:"schema_version"`
	ProjectID         *string         `json:"project_id"`
	Seed              *int64          `json:"seed"`
	DeadlockDetection *bool           `json:"deadlock_detection"`
	WorkerBudget      *int            `json:"worker_budget"`
	MemoryBudgetBytes *int64          `json:"memory_budget_bytes"`
	ClockOriginMs     *int64          `json:"clock_origin_ms"`
	LockModes         []v1LockMode    `json:"lock_modes"`
	Items             []v1Item        `json:"items"`
	Transactions      []v1Transaction `json:"transactions"`
}

func loadV1(data []byte) (*Project, error) {
	var doc v1Document
	if err := decodeStrict(data, &doc, "$"); err != nil {
		return nil, err
	}
	if doc.SchemaVersion == nil || *doc.SchemaVersion != 1 {
		version := 0
		if doc.SchemaVersion != nil {
			version = *doc.SchemaVersion
		}
		return nil, NewError(ESchemaUnsupported, "$.schema_version", "unsupported schema version %d", version)
	}
	if doc.ProjectID == nil || *doc.ProjectID == "" {
		return nil, NewError(EFieldRequired, "$.project_id", "project_id is required")
	}
	if doc.Seed == nil {
		return nil, NewError(EFieldRequired, "$.seed", "seed is required")
	}
	if doc.DeadlockDetection == nil {
		return nil, NewError(EFieldRequired, "$.deadlock_detection", "deadlock_detection is required in schema v1")
	}
	if doc.WorkerBudget == nil {
		return nil, NewError(EFieldRequired, "$.worker_budget", "worker_budget is required")
	}
	if doc.MemoryBudgetBytes == nil {
		return nil, NewError(EFieldRequired, "$.memory_budget_bytes", "memory_budget_bytes is required")
	}
	if doc.ClockOriginMs == nil {
		return nil, NewError(EFieldRequired, "$.clock_origin_ms", "clock_origin_ms is required")
	}
	codeModes, err := parseV1LockTable(doc.LockModes)
	if err != nil {
		return nil, err
	}
	project := &Project{
		ID: *doc.ProjectID, Seed: *doc.Seed,
		DeadlockDetection: *doc.DeadlockDetection,
		WorkerBudget:      *doc.WorkerBudget, MemoryBudgetBytes: *doc.MemoryBudgetBytes,
		ClockOriginMs: *doc.ClockOriginMs,
	}
	for i, raw := range doc.Items {
		path := fmt.Sprintf("$.items[%d]", i)
		if raw.ItemID == nil || *raw.ItemID == "" {
			return nil, NewError(EFieldRequired, path+".item_id", "item_id is required")
		}
		if raw.TableName == nil || *raw.TableName == "" {
			return nil, NewError(EFieldRequired, path+".table_name", "table_name is required")
		}
		project.Items = append(project.Items, Item{
			ID: *raw.ItemID, Table: *raw.TableName, InitialValue: raw.InitialValue,
		})
	}
	for i, raw := range doc.Transactions {
		txPath := fmt.Sprintf("$.transactions[%d]", i)
		if raw.TransactionID == nil || *raw.TransactionID == "" {
			return nil, NewError(EFieldRequired, txPath+".transaction_id", "transaction_id is required")
		}
		if raw.Priority == nil {
			return nil, NewError(EFieldRequired, txPath+".priority", "priority is required")
		}
		if raw.Isolation == nil {
			return nil, NewError(EFieldRequired, txPath+".isolation_level", "isolation_level is required")
		}
		isolation, ok := ParseIsolation(*raw.Isolation)
		if !ok {
			return nil, NewError(EIsolationUnknown, txPath+".isolation_level", "unknown isolation level %q", *raw.Isolation)
		}
		tx := Transaction{ID: *raw.TransactionID, Priority: *raw.Priority, Isolation: isolation}
		for j, rop := range raw.Operations {
			opPath := fmt.Sprintf("%s.operations[%d]", txPath, j)
			if rop.WaitTimeoutMs != nil && rop.WaitDeadlineMs != nil {
				return nil, NewError(ESchemaAmbiguous, opPath,
					"both wait_timeout_ms and wait_deadline_ms are present")
			}
			if rop.LocalSeq == nil {
				return nil, NewError(EFieldRequired, opPath+".local_seq", "local_seq is required")
			}
			if rop.AtMs == nil {
				return nil, NewError(EFieldRequired, opPath+".at_ms", "at_ms is required")
			}
			if rop.Kind == nil {
				return nil, NewError(EFieldRequired, opPath+".kind", "kind is required")
			}
			kind, ok := ParseOpKind(*rop.Kind)
			if !ok {
				return nil, NewError(EOperationInvalid, opPath+".kind", "unknown operation kind %q", *rop.Kind)
			}
			op := Operation{
				LocalSeq: *rop.LocalSeq, AtMs: *rop.AtMs, Kind: kind,
				ItemID: rop.ItemID, Table: rop.TableName, Value: rop.Value, NoWait: rop.NoWait,
			}
			if rop.Granularity != "" {
				granularity, ok := ParseGranularity(rop.Granularity)
				if !ok {
					return nil, NewError(EGranularityInvalid, opPath+".granularity", "unknown granularity %q", rop.Granularity)
				}
				op.Granularity = granularity
			}
			if rop.LockMode != "" {
				mode, ok := codeModes[rop.LockMode]
				if !ok {
					return nil, NewError(ELockModeUnknown, opPath+".lock_mode", "lock mode code %q is not in lock_modes", rop.LockMode)
				}
				op.Mode = mode
			}
			if rop.WaitTimeoutMs != nil {
				op.WaitTimeoutMs = *rop.WaitTimeoutMs
			} else if rop.WaitDeadlineMs != nil {
				return nil, NewError(ESchemaAmbiguous, opPath,
					"schema v1 only accepts relative wait_timeout_ms")
			}
			if op.NeedsLock() && rop.WaitTimeoutMs == nil {
				return nil, NewError(EFieldRequired, opPath+".wait_timeout_ms",
					"locking operation requires an explicit wait timeout")
			}
			tx.Ops = append(tx.Ops, op)
		}
		project.Transactions = append(project.Transactions, tx)
	}
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func parseV1LockTable(raw []v1LockMode) (map[string]LockMode, error) {
	if len(raw) == 0 {
		return nil, NewError(EFieldRequired, "$.lock_modes", "lock_modes table is required")
	}
	codes := make(map[string]LockMode)
	seenMode := make(map[LockMode]struct{})
	compat := make(map[LockMode]map[LockMode]bool)
	for i, row := range raw {
		path := fmt.Sprintf("$.lock_modes[%d]", i)
		if row.Code == nil || *row.Code == "" {
			return nil, NewError(EFieldRequired, path+".code", "code is required")
		}
		if row.Mode == nil || *row.Mode == "" {
			return nil, NewError(EFieldRequired, path+".mode", "mode is required")
		}
		mode, ok := ParseLockMode(*row.Mode)
		if !ok {
			return nil, NewError(ELockModeUnknown, path+".mode", "unknown lock mode %q", *row.Mode)
		}
		if _, exists := codes[*row.Code]; exists {
			return nil, NewError(ESchemaAmbiguous, path+".code", "duplicate lock mode code %q", *row.Code)
		}
		if _, exists := seenMode[mode]; exists {
			return nil, NewError(ESchemaAmbiguous, path+".mode", "mode %s appears more than once", mode)
		}
		codes[*row.Code] = mode
		seenMode[mode] = struct{}{}
		if compat[mode] == nil {
			compat[mode] = make(map[LockMode]bool)
		}
		for j, name := range row.CompatibleWith {
			other, ok := ParseLockMode(name)
			if !ok {
				return nil, NewError(ELockModeUnknown, fmt.Sprintf("%s.compatible_with[%d]", path, j),
					"unknown compatible mode %q", name)
			}
			compat[mode][other] = true
		}
	}
	if err := validateCompatibilityExpression(compat, "$.lock_modes"); err != nil {
		return nil, err
	}
	return codes, nil
}

type v2FeatureFlags struct {
	DeadlockDetection *bool `json:"deadlockDetection"`
}

type v2Item struct {
	ID           *string `json:"id"`
	Table        *string `json:"table"`
	InitialValue string  `json:"initialValue"`
}

type v2Operation struct {
	LocalSeq            *int     `json:"localSeq"`
	AtSeconds           *float64 `json:"atSeconds"`
	Kind                *string  `json:"type"`
	ItemID              string   `json:"itemId,omitempty"`
	TableName           string   `json:"tableName,omitempty"`
	Granularity         string   `json:"granularity,omitempty"`
	LockMode            string   `json:"lockMode,omitempty"`
	Value               string   `json:"value,omitempty"`
	WaitDeadlineSeconds *float64 `json:"waitDeadlineSeconds,omitempty"`
	WaitTimeoutMs       *float64 `json:"waitTimeoutMs,omitempty"`
	WaitDeadlineMs      *float64 `json:"waitDeadlineMs,omitempty"`
	NoWait              bool     `json:"noWait,omitempty"`
}

type v2Transaction struct {
	ID         *string       `json:"id"`
	Priority   *int          `json:"priority"`
	Isolation  *string       `json:"isolation"`
	Operations []v2Operation `json:"operations"`
}

type v2Document struct {
	Schema             *string         `json:"schema"`
	ProjectID          *string         `json:"projectId"`
	Seed               *int64          `json:"seed"`
	Features           *v2FeatureFlags `json:"features"`
	WorkerBudget       *int            `json:"workerBudget"`
	MemoryBudgetKiB    *int64          `json:"memoryBudgetKib"`
	ClockOriginSeconds *float64        `json:"clockOriginSeconds"`
	Items              []v2Item        `json:"items"`
	Transactions       []v2Transaction `json:"transactions"`
}

func loadV2(data []byte) (*Project, error) {
	var doc v2Document
	if err := decodeStrict(data, &doc, "$"); err != nil {
		return nil, err
	}
	if doc.Schema == nil {
		return nil, NewError(EFieldRequired, "$.schema", "schema is required")
	}
	if *doc.Schema != "lock-project/v2" && *doc.Schema != "lock-project/2" {
		return nil, NewError(ESchemaUnsupported, "$.schema", "unsupported schema %q", *doc.Schema)
	}
	if doc.ProjectID == nil || *doc.ProjectID == "" {
		return nil, NewError(EFieldRequired, "$.projectId", "projectId is required")
	}
	if doc.Seed == nil {
		return nil, NewError(EFieldRequired, "$.seed", "seed is required")
	}
	if doc.WorkerBudget == nil {
		return nil, NewError(EFieldRequired, "$.workerBudget", "workerBudget is required")
	}
	if doc.MemoryBudgetKiB == nil {
		return nil, NewError(EFieldRequired, "$.memoryBudgetKib", "memoryBudgetKib is required")
	}
	if *doc.MemoryBudgetKiB <= 0 || *doc.MemoryBudgetKiB > math.MaxInt64/1024 {
		return nil, NewError(EBackpressureMemory, "$.memoryBudgetKib", "memory budget is outside supported range")
	}
	if doc.ClockOriginSeconds == nil {
		return nil, NewError(EFieldRequired, "$.clockOriginSeconds", "clockOriginSeconds is required")
	}
	clockMs, err := secondsToMilliseconds(*doc.ClockOriginSeconds, "$.clockOriginSeconds")
	if err != nil {
		return nil, err
	}
	deadlock := false
	if doc.Features != nil && doc.Features.DeadlockDetection != nil {
		deadlock = *doc.Features.DeadlockDetection
	}
	project := &Project{
		ID: *doc.ProjectID, Seed: *doc.Seed,
		DeadlockDetection: deadlock,
		WorkerBudget:      *doc.WorkerBudget,
		MemoryBudgetBytes: *doc.MemoryBudgetKiB * 1024,
		ClockOriginMs:     clockMs,
	}
	for i, raw := range doc.Items {
		path := fmt.Sprintf("$.items[%d]", i)
		if raw.ID == nil || *raw.ID == "" {
			return nil, NewError(EFieldRequired, path+".id", "id is required")
		}
		if raw.Table == nil || *raw.Table == "" {
			return nil, NewError(EFieldRequired, path+".table", "table is required")
		}
		project.Items = append(project.Items, Item{ID: *raw.ID, Table: *raw.Table, InitialValue: raw.InitialValue})
	}
	for i, raw := range doc.Transactions {
		txPath := fmt.Sprintf("$.transactions[%d]", i)
		if raw.ID == nil || *raw.ID == "" {
			return nil, NewError(EFieldRequired, txPath+".id", "id is required")
		}
		if raw.Priority == nil {
			return nil, NewError(EFieldRequired, txPath+".priority", "priority is required")
		}
		if raw.Isolation == nil {
			return nil, NewError(EFieldRequired, txPath+".isolation", "isolation is required")
		}
		isolation, ok := ParseIsolation(*raw.Isolation)
		if !ok {
			return nil, NewError(EIsolationUnknown, txPath+".isolation", "unknown isolation level %q", *raw.Isolation)
		}
		tx := Transaction{ID: *raw.ID, Priority: *raw.Priority, Isolation: isolation}
		for j, rop := range raw.Operations {
			opPath := fmt.Sprintf("%s.operations[%d]", txPath, j)
			timeoutFields := 0
			if rop.WaitDeadlineSeconds != nil {
				timeoutFields++
			}
			if rop.WaitTimeoutMs != nil {
				timeoutFields++
			}
			if rop.WaitDeadlineMs != nil {
				timeoutFields++
			}
			if timeoutFields > 1 {
				return nil, NewError(ESchemaAmbiguous, opPath,
					"multiple wait timeout representations are present")
			}
			if rop.LocalSeq == nil {
				return nil, NewError(EFieldRequired, opPath+".localSeq", "localSeq is required")
			}
			if rop.AtSeconds == nil {
				return nil, NewError(EFieldRequired, opPath+".atSeconds", "atSeconds is required")
			}
			if rop.Kind == nil {
				return nil, NewError(EFieldRequired, opPath+".type", "type is required")
			}
			atMs, err := secondsToMilliseconds(*rop.AtSeconds, opPath+".atSeconds")
			if err != nil {
				return nil, err
			}
			kind, ok := ParseOpKind(*rop.Kind)
			if !ok {
				return nil, NewError(EOperationInvalid, opPath+".type", "unknown operation kind %q", *rop.Kind)
			}
			op := Operation{
				LocalSeq: *rop.LocalSeq, AtMs: atMs, Kind: kind,
				ItemID: rop.ItemID, Table: rop.TableName, Value: rop.Value, NoWait: rop.NoWait,
			}
			if rop.Granularity != "" {
				granularity, ok := ParseGranularity(rop.Granularity)
				if !ok {
					return nil, NewError(EGranularityInvalid, opPath+".granularity", "unknown granularity %q", rop.Granularity)
				}
				op.Granularity = granularity
			}
			if rop.LockMode != "" {
				mode, ok := ParseLockMode(rop.LockMode)
				if !ok {
					return nil, NewError(ELockModeUnknown, opPath+".lockMode", "unknown lock mode %q", rop.LockMode)
				}
				op.Mode = mode
			}
			if rop.WaitDeadlineSeconds != nil {
				deadlineMs, err := secondsToMilliseconds(*rop.WaitDeadlineSeconds, opPath+".waitDeadlineSeconds")
				if err != nil {
					return nil, err
				}
				if deadlineMs < atMs {
					return nil, NewError(ETimeoutReversed, opPath+".waitDeadlineSeconds",
						"wait deadline %d ms is before operation time %d ms", deadlineMs, atMs)
				}
				op.WaitTimeoutMs = deadlineMs - atMs
			} else if rop.WaitTimeoutMs != nil || rop.WaitDeadlineMs != nil {
				return nil, NewError(ESchemaAmbiguous, opPath,
					"schema v2 requires waitDeadlineSeconds and rejects millisecond timeout fields")
			}
			if op.NeedsLock() && rop.WaitDeadlineSeconds == nil {
				return nil, NewError(EFieldRequired, opPath+".waitDeadlineSeconds",
					"locking operation requires an explicit absolute wait deadline")
			}
			tx.Ops = append(tx.Ops, op)
		}
		project.Transactions = append(project.Transactions, tx)
	}
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func secondsToMilliseconds(value float64, path string) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, NewError(EUnitAmbiguous, path, "time value must be finite")
	}
	ms := value * 1000
	rounded := math.Round(ms)
	if math.Abs(ms-rounded) > 1e-6 {
		return 0, NewError(EUnitAmbiguous, path,
			"seconds value %v cannot be represented exactly as integer milliseconds", value)
	}
	if rounded > math.MaxInt64 || rounded < math.MinInt64 {
		return 0, NewError(EUnitAmbiguous, path, "time value is outside int64 millisecond range")
	}
	return int64(rounded), nil
}

type v3CompatNode struct {
	Group      string         `json:"Group,omitempty"`
	Mode       string         `json:"Mode,omitempty"`
	Compatible *bool          `json:"Compatible,omitempty"`
	Children   []v3CompatNode `json:"Children,omitempty"`
}

type v3Item struct {
	ID           *string `json:"ID"`
	Table        *string `json:"Table"`
	InitialValue string  `json:"InitialValue"`
}

type v3Operation struct {
	LocalSeq       *int    `json:"LocalSeq"`
	AtMs           *int64  `json:"AtMs"`
	Kind           *string `json:"Kind"`
	ItemID         string  `json:"ItemID,omitempty"`
	Table          string  `json:"Table,omitempty"`
	Granularity    string  `json:"Granularity,omitempty"`
	LockMode       *string `json:"LockMode,omitempty"`
	Value          string  `json:"Value,omitempty"`
	WaitTimeoutMs  *int64  `json:"WaitTimeoutMs,omitempty"`
	WaitDeadlineMs *int64  `json:"WaitDeadlineMs,omitempty"`
	NoWait         bool    `json:"NoWait,omitempty"`
}

type v3Transaction struct {
	ID         *string       `json:"ID"`
	Priority   *int          `json:"Priority"`
	Isolation  *string       `json:"Isolation"`
	Operations []v3Operation `json:"Operations"`
}

type v3Document struct {
	SchemaVersion     *int            `json:"SchemaVersion"`
	ProjectID         *string         `json:"ProjectID"`
	Seed              *int64          `json:"Seed"`
	DeadlockDetection *bool           `json:"DeadlockDetection"`
	WorkerBudget      *int            `json:"WorkerBudget"`
	MemoryBudgetBytes *int64          `json:"MemoryBudgetBytes"`
	ClockOriginMs     *int64          `json:"ClockOriginMs"`
	LockCompatibility *v3CompatNode   `json:"LockCompatibility"`
	Items             []v3Item        `json:"Items"`
	Transactions      []v3Transaction `json:"Transactions"`
}

func loadV3(data []byte) (*Project, error) {
	var doc v3Document
	if err := decodeStrict(data, &doc, "$"); err != nil {
		return nil, err
	}
	if doc.SchemaVersion == nil || *doc.SchemaVersion != 3 {
		version := 0
		if doc.SchemaVersion != nil {
			version = *doc.SchemaVersion
		}
		return nil, NewError(ESchemaUnsupported, "$.SchemaVersion", "unsupported schema version %d", version)
	}
	if doc.ProjectID == nil || *doc.ProjectID == "" {
		return nil, NewError(EFieldRequired, "$.ProjectID", "ProjectID is required")
	}
	if doc.Seed == nil {
		return nil, NewError(EFieldRequired, "$.Seed", "Seed is required")
	}
	if doc.WorkerBudget == nil {
		return nil, NewError(EFieldRequired, "$.WorkerBudget", "WorkerBudget is required")
	}
	if doc.MemoryBudgetBytes == nil {
		return nil, NewError(EFieldRequired, "$.MemoryBudgetBytes", "MemoryBudgetBytes is required")
	}
	if doc.ClockOriginMs == nil {
		return nil, NewError(EFieldRequired, "$.ClockOriginMs", "ClockOriginMs is required")
	}
	if doc.LockCompatibility == nil {
		return nil, NewError(EFieldRequired, "$.LockCompatibility", "LockCompatibility is required")
	}
	compat, modes, err := parseV3CompatibilityTree(doc.LockCompatibility, "$.LockCompatibility")
	if err != nil {
		return nil, err
	}
	if err := validateCompatibilityExpression(compat, "$.LockCompatibility"); err != nil {
		return nil, err
	}
	deadlock := false
	if doc.DeadlockDetection != nil {
		deadlock = *doc.DeadlockDetection
	}
	project := &Project{
		ID: *doc.ProjectID, Seed: *doc.Seed,
		DeadlockDetection: deadlock,
		WorkerBudget:      *doc.WorkerBudget, MemoryBudgetBytes: *doc.MemoryBudgetBytes,
		ClockOriginMs: *doc.ClockOriginMs,
	}
	for i, raw := range doc.Items {
		path := fmt.Sprintf("$.Items[%d]", i)
		if raw.ID == nil || *raw.ID == "" {
			return nil, NewError(EFieldRequired, path+".ID", "ID is required")
		}
		if raw.Table == nil || *raw.Table == "" {
			return nil, NewError(EFieldRequired, path+".Table", "Table is required")
		}
		project.Items = append(project.Items, Item{ID: *raw.ID, Table: *raw.Table, InitialValue: raw.InitialValue})
	}
	for i, raw := range doc.Transactions {
		txPath := fmt.Sprintf("$.Transactions[%d]", i)
		if raw.ID == nil || *raw.ID == "" {
			return nil, NewError(EFieldRequired, txPath+".ID", "ID is required")
		}
		if raw.Priority == nil {
			return nil, NewError(EFieldRequired, txPath+".Priority", "Priority is required")
		}
		if raw.Isolation == nil {
			return nil, NewError(EFieldRequired, txPath+".Isolation", "Isolation is required")
		}
		isolation, ok := ParseIsolation(*raw.Isolation)
		if !ok {
			return nil, NewError(EIsolationUnknown, txPath+".Isolation", "unknown isolation level %q", *raw.Isolation)
		}
		tx := Transaction{ID: *raw.ID, Priority: *raw.Priority, Isolation: isolation}
		for j, rop := range raw.Operations {
			opPath := fmt.Sprintf("%s.Operations[%d]", txPath, j)
			if rop.WaitTimeoutMs != nil && rop.WaitDeadlineMs != nil {
				return nil, NewError(ESchemaAmbiguous, opPath,
					"both WaitTimeoutMs and WaitDeadlineMs are present")
			}
			if rop.LocalSeq == nil {
				return nil, NewError(EFieldRequired, opPath+".LocalSeq", "LocalSeq is required")
			}
			if rop.AtMs == nil {
				return nil, NewError(EFieldRequired, opPath+".AtMs", "AtMs is required")
			}
			if rop.Kind == nil {
				return nil, NewError(EFieldRequired, opPath+".Kind", "Kind is required")
			}
			kind, ok := ParseOpKind(*rop.Kind)
			if !ok {
				return nil, NewError(EOperationInvalid, opPath+".Kind", "unknown operation kind %q", *rop.Kind)
			}
			op := Operation{
				LocalSeq: *rop.LocalSeq, AtMs: *rop.AtMs, Kind: kind,
				ItemID: rop.ItemID, Table: rop.Table, Value: rop.Value, NoWait: rop.NoWait,
			}
			if rop.Granularity != "" {
				granularity, ok := ParseGranularity(rop.Granularity)
				if !ok {
					return nil, NewError(EGranularityInvalid, opPath+".Granularity", "unknown granularity %q", rop.Granularity)
				}
				op.Granularity = granularity
			}
			if rop.LockMode != nil && *rop.LockMode != "" {
				if _, exists := modes[*rop.LockMode]; !exists {
					return nil, NewError(ELockModeUnknown, opPath+".LockMode",
						"lock mode %q is not declared in LockCompatibility", *rop.LockMode)
				}
				mode, _ := ParseLockMode(*rop.LockMode)
				op.Mode = mode
			}
			if rop.WaitTimeoutMs != nil {
				op.WaitTimeoutMs = *rop.WaitTimeoutMs
			} else if rop.WaitDeadlineMs != nil {
				return nil, NewError(ESchemaAmbiguous, opPath,
					"schema v3 only accepts relative WaitTimeoutMs")
			}
			if op.NeedsLock() && rop.WaitTimeoutMs == nil {
				return nil, NewError(EFieldRequired, opPath+".WaitTimeoutMs",
					"locking operation requires an explicit wait timeout")
			}
			tx.Ops = append(tx.Ops, op)
		}
		project.Transactions = append(project.Transactions, tx)
	}
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func parseV3CompatibilityTree(root *v3CompatNode, path string) (map[LockMode]map[LockMode]bool, map[string]struct{}, error) {
	compat := make(map[LockMode]map[LockMode]bool)
	declared := make(map[string]struct{})
	var walkGroups func(*v3CompatNode, string) error
	walkGroups = func(node *v3CompatNode, nodePath string) error {
		if node == nil {
			return NewError(EFieldRequired, nodePath, "compatibility node is null")
		}
		if node.Mode != "" {
			rowMode, ok := ParseLockMode(node.Mode)
			if !ok {
				return NewError(ELockModeUnknown, nodePath+".Mode", "unknown lock mode %q", node.Mode)
			}
			if len(node.Children) == 0 {
				return NewError(EFieldRequired, nodePath+".Children",
					"mode row %s needs compatibility children", node.Mode)
			}
			if _, exists := declared[node.Mode]; exists {
				return NewError(ESchemaAmbiguous, nodePath+".Mode", "mode row %q appears more than once", node.Mode)
			}
			declared[node.Mode] = struct{}{}
			row := make(map[LockMode]bool)
			seen := make(map[LockMode]struct{})
			for i, child := range node.Children {
				childPath := fmt.Sprintf("%s.Children[%d]", nodePath, i)
				if child.Group != "" || len(child.Children) != 0 {
					return NewError(ESchemaAmbiguous, childPath, "compatibility cell must be a mode leaf")
				}
				if child.Mode == "" || child.Compatible == nil {
					return NewError(EFieldRequired, childPath, "compatibility cell needs Mode and Compatible")
				}
				other, ok := ParseLockMode(child.Mode)
				if !ok {
					return NewError(ELockModeUnknown, childPath+".Mode", "unknown lock mode %q", child.Mode)
				}
				if _, exists := seen[other]; exists {
					return NewError(ESchemaAmbiguous, childPath+".Mode", "duplicate compatibility cell %q", child.Mode)
				}
				seen[other] = struct{}{}
				row[other] = *child.Compatible
			}
			compat[rowMode] = row
			return nil
		}
		if node.Group == "" {
			return NewError(EFieldRequired, nodePath, "tree node needs Group or Mode")
		}
		if node.Compatible != nil {
			return NewError(ESchemaAmbiguous, nodePath+".Compatible", "group node cannot carry Compatible")
		}
		if len(node.Children) == 0 {
			return NewError(EFieldRequired, nodePath+".Children", "group node needs children")
		}
		for i := range node.Children {
			if err := walkGroups(&node.Children[i], fmt.Sprintf("%s.Children[%d]", nodePath, i)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walkGroups(root, path); err != nil {
		return nil, nil, err
	}
	if len(declared) != len(AllLockModes) {
		return nil, nil, NewError(ELockModeCompatMismatch, path,
			"compatibility tree must declare exactly the five supported modes")
	}
	return compat, declared, nil
}

func validateCompatibilityExpression(compat map[LockMode]map[LockMode]bool, path string) error {
	if len(compat) != len(AllLockModes) {
		return NewError(ELockModeCompatMismatch, path,
			"compatibility expression must declare %d modes, got %d", len(AllLockModes), len(compat))
	}
	for _, left := range AllLockModes {
		row, ok := compat[left]
		if !ok {
			return NewError(ELockModeCompatMismatch, path, "missing compatibility row for mode %s", left)
		}
		for _, right := range AllLockModes {
			got := row[right]
			want := CanonicalLockCompatibility[left][right]
			if got != want {
				return NewError(ELockModeCompatMismatch, path,
					"mode pair %s/%s is %t, canonical relation requires %t", left, right, got, want)
			}
		}
	}
	return nil
}

// ValidateProject applies normalized-domain invariants after schema loading.
func ValidateProject(p *Project) error {
	if p == nil {
		return NewError(EFieldRequired, "$", "project is nil")
	}
	if strings.TrimSpace(p.ID) == "" {
		return NewError(EFieldRequired, "$.id", "project id is required")
	}
	if p.WorkerBudget < 1 || p.WorkerBudget > MaxWorkers {
		return NewError(EBackpressureWorkers, "$.worker_budget",
			"worker budget %d is outside [1,%d]", p.WorkerBudget, MaxWorkers)
	}
	if p.MemoryBudgetBytes < MinWorkerMemoryBytes {
		return NewError(EBackpressureMemory, "$.memory_budget_bytes",
			"memory budget %d is below minimum %d", p.MemoryBudgetBytes, MinWorkerMemoryBytes)
	}
	if len(p.Items) == 0 {
		return NewError(EFieldRequired, "$.items", "at least one data item is required")
	}
	itemIDs := make(map[string]Item)
	tables := make(map[string]struct{})
	for i, item := range p.Items {
		path := fmt.Sprintf("$.items[%d]", i)
		if strings.TrimSpace(item.ID) == "" {
			return NewError(EFieldRequired, path+".id", "item id is required")
		}
		if strings.TrimSpace(item.Table) == "" {
			return NewError(EFieldRequired, path+".table", "item table is required")
		}
		if _, exists := itemIDs[item.ID]; exists {
			return NewError(EDuplicateItem, path+".id", "duplicate data item %q", item.ID)
		}
		itemIDs[item.ID] = item
		tables[item.Table] = struct{}{}
	}
	if len(p.Transactions) == 0 {
		return NewError(EFieldRequired, "$.transactions", "at least one transaction is required")
	}
	txIDs := make(map[string]struct{})
	for i, tx := range p.Transactions {
		txPath := fmt.Sprintf("$.transactions[%d]", i)
		if strings.TrimSpace(tx.ID) == "" {
			return NewError(EFieldRequired, txPath+".id", "transaction id is required")
		}
		if _, exists := txIDs[tx.ID]; exists {
			return NewError(EDuplicateTransaction, txPath+".id", "duplicate transaction %q", tx.ID)
		}
		txIDs[tx.ID] = struct{}{}
		if _, ok := ParseIsolation(string(tx.Isolation)); !ok {
			return NewError(EIsolationUnknown, txPath+".isolation", "unknown isolation level %q", tx.Isolation)
		}
		if len(tx.Ops) == 0 {
			return NewError(EFieldRequired, txPath+".operations", "transaction has no operations")
		}
		terminated := false
		lastAt := int64(-1)
		for j, op := range tx.Ops {
			opPath := fmt.Sprintf("%s.operations[%d]", txPath, j)
			if terminated {
				return NewError(EOperationAfterTerminal, opPath, "operation follows COMMIT/ROLLBACK")
			}
			if op.LocalSeq != j+1 {
				return NewError(EOperationOrder, opPath+".local_seq",
					"local_seq must be consecutive from 1; want %d got %d", j+1, op.LocalSeq)
			}
			if op.AtMs < 0 {
				return NewError(ETimeoutReversed, opPath+".at_ms", "operation time cannot be negative")
			}
			if op.AtMs < lastAt {
				return NewError(EOperationOrder, opPath+".at_ms",
					"operation time %d is before previous operation time %d", op.AtMs, lastAt)
			}
			lastAt = op.AtMs
			switch op.Kind {
			case OpRead, OpWrite, OpLock, OpCommit, OpRollback:
			default:
				return NewError(EOperationInvalid, opPath+".kind", "unknown operation kind %q", op.Kind)
			}
			if op.Mode != "" {
				if _, ok := ParseLockMode(string(op.Mode)); !ok {
					return NewError(ELockModeUnknown, opPath+".mode", "unknown lock mode %q", op.Mode)
				}
			}
			if op.Granularity != "" {
				if _, ok := ParseGranularity(string(op.Granularity)); !ok {
					return NewError(EGranularityInvalid, opPath+".granularity",
						"unknown granularity %q", op.Granularity)
				}
			}
			if op.Kind == OpCommit || op.Kind == OpRollback {
				if j != len(tx.Ops)-1 {
					return NewError(EOperationAfterTerminal, opPath, "COMMIT/ROLLBACK must be the final operation")
				}
				if op.Mode != "" || op.ItemID != "" || op.Table != "" || op.Granularity != "" ||
					op.WaitTimeoutMs != 0 || op.NoWait {
					return NewError(EOperationInvalid, opPath, "terminal operation cannot carry a lock target")
				}
				terminated = true
				continue
			}
			if op.WaitTimeoutMs < 0 {
				return NewError(ETimeoutReversed, opPath+".wait_timeout_ms", "wait timeout cannot be negative")
			}
			if op.AtMs > math.MaxInt64-op.WaitTimeoutMs {
				return NewError(ETimeoutReversed, opPath+".wait_timeout_ms",
					"wait deadline overflows int64 milliseconds")
			}
			switch op.Kind {
			case OpRead, OpWrite:
				item, ok := itemIDs[op.ItemID]
				if !ok {
					return NewError(EUnknownItem, opPath+".item_id", "unknown data item %q", op.ItemID)
				}
				if op.Table == "" {
					p.Transactions[i].Ops[j].Table = item.Table
				} else if op.Table != item.Table {
					return NewError(EUnknownTable, opPath+".table",
						"item %q belongs to table %q, not %q", op.ItemID, item.Table, op.Table)
				}
				if op.Granularity == "" {
					p.Transactions[i].Ops[j].Granularity = RowGranularity
				}
			case OpLock:
				if op.Mode == "" {
					return NewError(EOperationInvalid, opPath+".mode", "LOCK requires a lock mode")
				}
				if op.Granularity == "" {
					return NewError(EGranularityInvalid, opPath+".granularity", "LOCK requires a granularity")
				}
				if op.Granularity == RowGranularity {
					if _, ok := itemIDs[op.ItemID]; !ok {
						return NewError(EUnknownItem, opPath+".item_id", "unknown row item %q", op.ItemID)
					}
					if op.Table == "" {
						item, _ := itemIDs[op.ItemID]
						p.Transactions[i].Ops[j].Table = item.Table
					} else if item, ok := itemIDs[op.ItemID]; ok && item.Table != op.Table {
						return NewError(EUnknownTable, opPath+".table",
							"item %q belongs to table %q, not %q", op.ItemID, item.Table, op.Table)
					}
				} else {
					table := op.Table
					if table == "" && op.ItemID != "" {
						if item, ok := itemIDs[op.ItemID]; ok {
							table = item.Table
							p.Transactions[i].Ops[j].Table = table
						}
					}
					if table == "" {
						return NewError(EUnknownTable, opPath+".table", "table lock requires a table")
					}
					if _, ok := tables[table]; !ok {
						return NewError(EUnknownTable, opPath+".table", "unknown table %q", table)
					}
				}
			}
			if op.NeedsLock() && op.WaitTimeoutMs == 0 {
				// Zero is a valid immediate timeout, but only when the field was
				// explicitly represented. Schema loaders distinguish absence.
			}
			if op.Kind == OpLock && op.Granularity == "" {
				return NewError(EGranularityInvalid, opPath+".granularity", "lock granularity is required")
			}
		}
		if !terminated {
			return NewError(EOperationOrder, txPath+".operations",
				"transaction %q does not end in COMMIT or ROLLBACK", tx.ID)
		}
	}
	// Validation changes only normalized derived fields (table/row defaults)
	// inside the supplied project. The caller supplies a freshly decoded copy.
	return nil
}
