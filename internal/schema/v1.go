package schema

import (
	"encoding/json"

	"mvccworkbench/internal/domain"
)

const V1Name = "flat-global-tick"

type EnvelopeV1 struct {
	SchemaVersion string      `json:"schema_version"`
	Encoding      string      `json:"encoding"`
	ProjectID     string      `json:"project_id"`
	BatchID       string      `json:"batch_id"`
	Seed          int64       `json:"seed"`
	Generator     string      `json:"generator_version"`
	Rules         RulesV1     `json:"rules"`
	Items         []ItemV1    `json:"data_items"`
	Transactions  []TxnV1     `json:"transactions"`
	Versions      []VersionV1 `json:"versions"`
	Operations    []OpV1      `json:"operations"`
}

type RulesV1 struct {
	CommitTieRule string `json:"commit_tie_rule"`
	ConflictRule  string `json:"conflict_rule"`
}

type ItemV1 struct {
	ItemID      string `json:"item_id"`
	Description string `json:"description,omitempty"`
}

type TxnV1 struct {
	TxnID             uint64 `json:"txn_id"`
	TxnRef            string `json:"txn_ref"`
	StartTick         int64  `json:"start_tick"`
	CommitTick        *int64 `json:"commit_tick"`
	RollbackTick      *int64 `json:"rollback_tick"`
	IsolationLevel    string `json:"isolation_level"`
	RelativeCommitSeq int    `json:"relative_commit_seq,omitempty"`
}

type VersionV1 struct {
	VersionID string  `json:"version_id"`
	ItemID    string  `json:"item_id"`
	CreateTxn uint64  `json:"create_txn"`
	DeleteTxn *uint64 `json:"delete_txn"`
	Digest    string  `json:"content_digest"`
	WriteGen  int     `json:"write_algebra"`
	ChainPos  int     `json:"chain_position"`
}

type OpV1 struct {
	Index     int    `json:"op_index"`
	OpID      string `json:"op_id"`
	OpKind    string `json:"op_kind"`
	TxnID     uint64 `json:"txn_id,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	AtTick    int64  `json:"at_tick"`
	VersionID string `json:"new_version_id,omitempty"`
	Digest    string `json:"content_digest,omitempty"`
	WriteGen  int    `json:"write_algebra,omitempty"`
	GCStepID  string `json:"gc_step_id,omitempty"`
}

func EncodeV1(project *domain.Project) ([]byte, error) {
	doc := EnvelopeV1{
		SchemaVersion: domain.SchemaV1,
		Encoding:      V1Name,
		ProjectID:     project.ProjectID,
		BatchID:       project.BatchID,
		Seed:          project.Seed,
		Generator:     project.Generator,
		Rules: RulesV1{
			CommitTieRule: string(project.Rules.CommitTieRule),
			ConflictRule:  string(project.Rules.ConflictRule),
		},
		Items:        make([]ItemV1, 0, len(project.Items)),
		Transactions: make([]TxnV1, 0, len(project.Txns)),
		Versions:     make([]VersionV1, 0, len(project.Versions)),
		Operations:   make([]OpV1, 0, len(project.Ops)),
	}
	for _, item := range project.Items {
		doc.Items = append(doc.Items, ItemV1{ItemID: item.ID, Description: item.Description})
	}
	for _, txn := range project.Txns {
		var commitTick, rollbackTick *int64
		if txn.CommitMS != nil {
			v := *txn.CommitMS / domain.TickMilliseconds
			commitTick = &v
		}
		if txn.RollbackMS != nil {
			v := *txn.RollbackMS / domain.TickMilliseconds
			rollbackTick = &v
		}
		doc.Transactions = append(doc.Transactions, TxnV1{
			TxnID:             txn.ID,
			TxnRef:            txn.Ref,
			StartTick:         txn.StartMS / domain.TickMilliseconds,
			CommitTick:        commitTick,
			RollbackTick:      rollbackTick,
			IsolationLevel:    string(txn.Isolation),
			RelativeCommitSeq: txn.RelativeCommitSeq,
		})
	}
	for _, version := range project.Versions {
		deleteTxn := version.DeleteTxn
		doc.Versions = append(doc.Versions, VersionV1{
			VersionID: version.ID,
			ItemID:    version.ItemID,
			CreateTxn: version.CreateTxn,
			DeleteTxn: deleteTxn,
			Digest:    version.Digest,
			WriteGen:  version.WriteGen,
			ChainPos:  version.ChainPos,
		})
	}
	for _, op := range project.Ops {
		doc.Operations = append(doc.Operations, OpV1{
			Index:     op.Index,
			OpID:      op.ID,
			OpKind:    string(op.Kind),
			TxnID:     op.TxnID,
			ItemID:    op.ItemID,
			AtTick:    op.AtMS / domain.TickMilliseconds,
			VersionID: op.VersionID,
			Digest:    op.Digest,
			WriteGen:  op.WriteGen,
			GCStepID:  op.GCStepID,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func loadV1(data []byte) (*domain.Project, error) {
	var doc EnvelopeV1
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, domain.NewError(domain.ESchemaUnknown, "v1 JSON cannot be decoded").
			At("schema[v1]").With("decode_error", err.Error())
	}
	if doc.Encoding != "" && doc.Encoding != V1Name {
		return nil, domain.NewError(domain.ESchemaBadCombination, "unrecognized v1 encoding combination").
			At("encoding").With("value", doc.Encoding).With("expected", V1Name)
	}

	project := &domain.Project{
		SchemaVersion: domain.ProjectModelVersion,
		SourceSchema:  domain.SchemaV1,
		ProjectID:     doc.ProjectID,
		BatchID:       doc.BatchID,
		Seed:          doc.Seed,
		Generator:     doc.Generator,
		Rules: domain.Rules{
			CommitTieRule: domain.CommitTieRule(doc.Rules.CommitTieRule),
			ConflictRule:  domain.ConflictRule(doc.Rules.ConflictRule),
			// All external units are normalized to logical milliseconds.
			TimeUnit: "millisecond",
		},
		Items: make([]domain.Item, 0, len(doc.Items)),
		Txns:  make([]domain.Txn, 0, len(doc.Transactions)),
	}
	for _, item := range doc.Items {
		project.Items = append(project.Items, domain.Item{ID: item.ItemID, Description: item.Description})
	}
	for _, txn := range doc.Transactions {
		var commitMS, rollbackMS *int64
		if txn.CommitTick != nil {
			v := *txn.CommitTick * domain.TickMilliseconds
			commitMS = &v
		}
		if txn.RollbackTick != nil {
			v := *txn.RollbackTick * domain.TickMilliseconds
			rollbackMS = &v
		}
		project.Txns = append(project.Txns, domain.Txn{
			ID:                txn.TxnID,
			Ref:               txn.TxnRef,
			StartMS:           txn.StartTick * domain.TickMilliseconds,
			CommitMS:          commitMS,
			RollbackMS:        rollbackMS,
			Isolation:         domain.Isolation(txn.IsolationLevel),
			RelativeCommitSeq: txn.RelativeCommitSeq,
		})
	}
	for _, version := range doc.Versions {
		deleteTxn := version.DeleteTxn
		if deleteTxn != nil && *deleteTxn == domain.NoTxn {
			deleteTxn = nil
		}
		project.Versions = append(project.Versions, domain.Version{
			ID:        version.VersionID,
			ItemID:    version.ItemID,
			CreateTxn: version.CreateTxn,
			DeleteTxn: deleteTxn,
			Digest:    version.Digest,
			WriteGen:  version.WriteGen,
			ChainPos:  version.ChainPos,
		})
	}
	for _, op := range doc.Operations {
		project.Ops = append(project.Ops, domain.Op{
			Index:     op.Index,
			ID:        op.OpID,
			Kind:      domain.OpKind(op.OpKind),
			TxnID:     op.TxnID,
			ItemID:    op.ItemID,
			AtMS:      op.AtTick * domain.TickMilliseconds,
			VersionID: op.VersionID,
			Digest:    op.Digest,
			WriteGen:  op.WriteGen,
			GCStepID:  op.GCStepID,
		})
	}
	if err := project.Validate(); err != nil {
		return nil, err
	}
	return project, nil
}
