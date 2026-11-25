package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	"mvccworkbench/internal/domain"
)

const V3Name = "tree-global-ms-no-delete-field"

type EnvelopeV3 struct {
	SchemaVersion       string    `json:"schema-version"`
	Encoding            string    `json:"encoding"`
	ProjectID           string    `json:"project-id"`
	BatchID             string    `json:"batch-id"`
	Seed                int64     `json:"seed"`
	GeneratorVersion    string    `json:"generator-version"`
	CommitTieRule       string    `json:"commit-tie-rule"`
	WriteConflictPolicy string    `json:"write-conflict-policy"`
	DataItems           []ItemV3  `json:"data-items"`
	Transactions        []TxnV3   `json:"transactions"`
	VersionChains       []ChainV3 `json:"version-chains"`
	OperationStream     []OpV3    `json:"operation-stream"`
}

type ItemV3 struct {
	ID          string `json:"item-id"`
	Description string `json:"description,omitempty"`
}

type TxnV3 struct {
	ID             uint64 `json:"txn-id"`
	Ref            string `json:"txn-ref"`
	StartMS        int64  `json:"start-ms"`
	CommitMS       *int64 `json:"commit-ms,omitempty"`
	RollbackMS     *int64 `json:"rollback-ms,omitempty"`
	IsolationLevel string `json:"isolation-level"`
	CommitSequence int    `json:"commit-sequence,omitempty"`
}

type ChainV3 struct {
	ItemID   string      `json:"item-id"`
	Versions []VersionV3 `json:"versions"`
}

// VersionV3 deliberately has no delete-transaction field. Deletion markers are
// recovered from the next chain node's create transaction.
type VersionV3 struct {
	ID       string `json:"version-id"`
	CreateTx uint64 `json:"create-txn"`
	Digest   string `json:"digest"`
	Algebra  int    `json:"write-algebra"`
	Position int    `json:"chain-position"`
}

type OpV3 struct {
	Index     int    `json:"op-index"`
	ID        string `json:"op-id"`
	Kind      string `json:"op-kind"`
	TxnID     uint64 `json:"txn-id,omitempty"`
	ItemID    string `json:"item-id,omitempty"`
	AtMS      int64  `json:"at-ms"`
	VersionID string `json:"version-id,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Algebra   int    `json:"write-algebra,omitempty"`
	GCStepID  string `json:"gc-step-id,omitempty"`
}

func EncodeV3(project *domain.Project) ([]byte, error) {
	doc := EnvelopeV3{
		SchemaVersion:       domain.SchemaV3,
		Encoding:            V3Name,
		ProjectID:           project.ProjectID,
		BatchID:             project.BatchID,
		Seed:                project.Seed,
		GeneratorVersion:    project.Generator,
		CommitTieRule:       string(project.Rules.CommitTieRule),
		WriteConflictPolicy: string(project.Rules.ConflictRule),
		DataItems:           []ItemV3{},
		Transactions:        []TxnV3{},
		VersionChains:       []ChainV3{},
		OperationStream:     []OpV3{},
	}
	for _, item := range project.Items {
		doc.DataItems = append(doc.DataItems, ItemV3{ID: item.ID, Description: item.Description})
	}
	for _, txn := range project.Txns {
		doc.Transactions = append(doc.Transactions, TxnV3{
			ID:             txn.ID,
			Ref:            txn.Ref,
			StartMS:        txn.StartMS,
			CommitMS:       txn.CommitMS,
			RollbackMS:     txn.RollbackMS,
			IsolationLevel: string(txn.Isolation),
			CommitSequence: txn.RelativeCommitSeq,
		})
	}
	for _, item := range project.Items {
		versions := project.VersionsForItem(item.ID)
		chain := ChainV3{ItemID: item.ID, Versions: []VersionV3{}}
		for _, version := range versions {
			chain.Versions = append(chain.Versions, VersionV3{
				ID:       version.ID,
				CreateTx: version.CreateTxn,
				Digest:   version.Digest,
				Algebra:  version.WriteGen,
				Position: version.ChainPos,
			})
		}
		doc.VersionChains = append(doc.VersionChains, chain)
	}
	for _, op := range project.Ops {
		doc.OperationStream = append(doc.OperationStream, OpV3{
			Index:     op.Index,
			ID:        op.ID,
			Kind:      string(op.Kind),
			TxnID:     op.TxnID,
			ItemID:    op.ItemID,
			AtMS:      op.AtMS,
			VersionID: op.VersionID,
			Digest:    op.Digest,
			Algebra:   op.WriteGen,
			GCStepID:  op.GCStepID,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func loadV3(data []byte) (*domain.Project, error) {
	var doc EnvelopeV3
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, domain.NewError(domain.ESchemaUnknown, "v3 JSON cannot be decoded").
			At("schema[v3]").With("decode_error", err.Error())
	}
	if doc.Encoding != V3Name {
		return nil, domain.NewError(domain.ESchemaBadCombination, "unrecognized v3 encoding combination").
			At("encoding").With("value", doc.Encoding).With("expected", V3Name)
	}
	project := &domain.Project{
		SchemaVersion: domain.ProjectModelVersion,
		SourceSchema:  domain.SchemaV3,
		ProjectID:     doc.ProjectID,
		BatchID:       doc.BatchID,
		Seed:          doc.Seed,
		Generator:     doc.GeneratorVersion,
		Rules: domain.Rules{
			CommitTieRule: domain.CommitTieRule(doc.CommitTieRule),
			ConflictRule:  domain.ConflictRule(doc.WriteConflictPolicy),
			TimeUnit:      "millisecond",
		},
	}
	for _, item := range doc.DataItems {
		project.Items = append(project.Items, domain.Item{ID: item.ID, Description: item.Description})
	}
	for _, txn := range doc.Transactions {
		project.Txns = append(project.Txns, domain.Txn{
			ID:                txn.ID,
			Ref:               txn.Ref,
			StartMS:           txn.StartMS,
			CommitMS:          txn.CommitMS,
			RollbackMS:        txn.RollbackMS,
			Isolation:         domain.Isolation(txn.IsolationLevel),
			RelativeCommitSeq: txn.CommitSequence,
		})
	}

	for chainIndex, chain := range doc.VersionChains {
		versions := append([]VersionV3(nil), chain.Versions...)
		sort.SliceStable(versions, func(i, j int) bool {
			if versions[i].Position != versions[j].Position {
				return versions[i].Position < versions[j].Position
			}
			return versions[i].ID < versions[j].ID
		})
		for i, node := range versions {
			version := domain.Version{
				ID:        node.ID,
				ItemID:    chain.ItemID,
				CreateTxn: node.CreateTx,
				Digest:    node.Digest,
				WriteGen:  node.Algebra,
				ChainPos:  node.Position,
			}
			if i+1 < len(versions) {
				// In the write-append invariant, a predecessor is marked by
				// the transaction that created its immediate successor.
				deleteTxn := versions[i+1].CreateTx
				version.DeleteTxn = &deleteTxn
			}
			project.Versions = append(project.Versions, version)
		}
		_ = chainIndex
	}
	for _, op := range doc.OperationStream {
		project.Ops = append(project.Ops, domain.Op{
			Index:     op.Index,
			ID:        op.ID,
			Kind:      domain.OpKind(op.Kind),
			TxnID:     op.TxnID,
			ItemID:    op.ItemID,
			AtMS:      op.AtMS,
			VersionID: op.VersionID,
			Digest:    op.Digest,
			WriteGen:  op.Algebra,
			GCStepID:  op.GCStepID,
		})
	}
	if err := project.Validate(); err != nil {
		return nil, err
	}
	if err := validateV3DeleteInvariant(project); err != nil {
		return nil, err
	}
	return project, nil
}

func validateV3DeleteInvariant(project *domain.Project) error {
	for _, item := range project.Items {
		versions := project.VersionsForItem(item.ID)
		for i := 0; i+1 < len(versions); i++ {
			got := domain.NoTxn
			if versions[i].DeleteTxn != nil {
				got = *versions[i].DeleteTxn
			}
			if got != versions[i+1].CreateTxn {
				return domain.NewError(domain.ESchemaBadCombination,
					"v3 without delete field requires successor-create deletion encoding").
					At(fmt.Sprintf("version-chains[%s].versions[%s]", item.ID, versions[i].ID)).
					With("item_id", item.ID).
					With("version_id", versions[i].ID).
					With("inferred_delete_txn", versions[i+1].CreateTxn).
					With("canonical_delete_txn", got)
			}
		}
	}
	return nil
}
