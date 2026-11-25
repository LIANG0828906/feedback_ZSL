package schema

import (
	"encoding/json"
	"fmt"

	"mvccworkbench/internal/domain"
)

const V2Name = "grouped-relative-seq-ms"

type EnvelopeV2 struct {
	SchemaVersion        string        `json:"schemaVersion"`
	Encoding             string        `json:"encoding"`
	NumberingBasis       string        `json:"numberingBasis"`
	TimeUnit             string        `json:"timeUnit"`
	ChainOrganization    string        `json:"chainOrganization"`
	ProjectID            string        `json:"projectId"`
	BatchID              string        `json:"batchId"`
	Seed                 int64         `json:"seed"`
	GeneratorVersion     string        `json:"generatorVersion"`
	DeclarationRules     RulesV2       `json:"declarationRules"`
	DataItems            []ItemV2      `json:"dataItems"`
	TransactionDirectory []TxnV2       `json:"transactionDirectory"`
	VersionChainTrees    []ChainTreeV2 `json:"versionChainTrees"`
	OperationLog         []OpV2        `json:"operationLog"`
}

type RulesV2 struct {
	CommitTieAdjudication string `json:"commitTieAdjudication"`
	WriteConflictPolicy   string `json:"writeConflictPolicy"`
}

type ItemV2 struct {
	DataItemID  string `json:"dataItemId"`
	Description string `json:"description,omitempty"`
}

type TxnRefV2 struct {
	CommitSequence int    `json:"commitSequence,omitempty"`
	TxnAlias       string `json:"txnAlias,omitempty"`
}

type TxnV2 struct {
	TxnAlias             string `json:"txnAlias"`
	CommitSequence       int    `json:"commitSequence,omitempty"`
	StartMilliseconds    int64  `json:"startMilliseconds"`
	CommitMilliseconds   *int64 `json:"commitMilliseconds,omitempty"`
	RollbackMilliseconds *int64 `json:"rollbackMilliseconds,omitempty"`
	IsolationLevel       string `json:"isolationLevel"`
}

type ChainTreeV2 struct {
	DataItemID    string      `json:"dataItemId"`
	HeadVersionID string      `json:"headVersionId"`
	Nodes         []VersionV2 `json:"nodes"`
}

type VersionV2 struct {
	VersionID     string    `json:"versionId"`
	CreatedBy     TxnRefV2  `json:"createdBy"`
	RemovedBy     *TxnRefV2 `json:"removedBy,omitempty"`
	ContentDigest string    `json:"contentDigest"`
	WriteAlgebra  int       `json:"writeAlgebra"`
	ChainOrder    int       `json:"chainOrder"`
}

type OpV2 struct {
	SequenceNo      int      `json:"sequenceNo"`
	OperationID     string   `json:"operationId"`
	OperationType   string   `json:"operationType"`
	Transaction     TxnRefV2 `json:"transaction,omitempty"`
	DataItemID      string   `json:"dataItemId,omitempty"`
	AtMilliseconds  int64    `json:"atMilliseconds"`
	NewVersionID    string   `json:"newVersionId,omitempty"`
	ContentDigest   string   `json:"contentDigest,omitempty"`
	WriteAlgebra    int      `json:"writeAlgebra,omitempty"`
	GCMaintenanceID string   `json:"gcMaintenanceId,omitempty"`
}

type refResolver struct {
	byAlias map[string]uint64
	bySeq   map[int]uint64
}

func newRefResolver(project *domain.Project) refResolver {
	r := refResolver{byAlias: map[string]uint64{}, bySeq: map[int]uint64{}}
	for _, txn := range project.Txns {
		r.byAlias[txn.Ref] = txn.ID
		if txn.RelativeCommitSeq > 0 {
			r.bySeq[txn.RelativeCommitSeq] = txn.ID
		}
	}
	return r
}

func (r refResolver) resolve(ref TxnRefV2, path string) (uint64, error) {
	if ref.CommitSequence > 0 && ref.TxnAlias != "" {
		return 0, domain.NewError(domain.ESchemaBadCombination, "transaction reference has both commit sequence and alias").
			At(path).With("commit_sequence", ref.CommitSequence).With("txn_alias", ref.TxnAlias)
	}
	if ref.CommitSequence > 0 {
		id, ok := r.bySeq[ref.CommitSequence]
		if !ok {
			return 0, domain.NewError(domain.EInputInvalidRef, "unknown relative commit sequence").
				At(path).With("commit_sequence", ref.CommitSequence)
		}
		return id, nil
	}
	if ref.TxnAlias != "" {
		id, ok := r.byAlias[ref.TxnAlias]
		if !ok {
			return 0, domain.NewError(domain.EInputInvalidRef, "unknown transaction alias").
				At(path).With("txn_alias", ref.TxnAlias)
		}
		return id, nil
	}
	return 0, domain.NewError(domain.EInputInvalidRef, "empty transaction reference").At(path)
}

func encodeRefV2(txn domain.Txn) TxnRefV2 {
	if txn.RelativeCommitSeq > 0 && txn.CommitMS != nil {
		return TxnRefV2{CommitSequence: txn.RelativeCommitSeq}
	}
	return TxnRefV2{TxnAlias: txn.Ref}
}

func EncodeV2(project *domain.Project) ([]byte, error) {
	resolver := newRefResolver(project)
	_ = resolver
	doc := EnvelopeV2{
		SchemaVersion:     domain.SchemaV2,
		Encoding:          V2Name,
		NumberingBasis:    "RELATIVE_COMMIT_SEQUENCE_WITH_ALIAS_FALLBACK",
		TimeUnit:          "MILLISECOND",
		ChainOrganization: "GROUPED_TREE",
		ProjectID:         project.ProjectID,
		BatchID:           project.BatchID,
		Seed:              project.Seed,
		GeneratorVersion:  project.Generator,
		DeclarationRules: RulesV2{
			CommitTieAdjudication: string(project.Rules.CommitTieRule),
			WriteConflictPolicy:   string(project.Rules.ConflictRule),
		},
		DataItems:            []ItemV2{},
		TransactionDirectory: []TxnV2{},
		VersionChainTrees:    []ChainTreeV2{},
		OperationLog:         []OpV2{},
	}
	for _, item := range project.Items {
		doc.DataItems = append(doc.DataItems, ItemV2{DataItemID: item.ID, Description: item.Description})
	}
	for _, txn := range project.Txns {
		doc.TransactionDirectory = append(doc.TransactionDirectory, TxnV2{
			TxnAlias:             txn.Ref,
			CommitSequence:       txn.RelativeCommitSeq,
			StartMilliseconds:    txn.StartMS,
			CommitMilliseconds:   txn.CommitMS,
			RollbackMilliseconds: txn.RollbackMS,
			IsolationLevel:       string(txn.Isolation),
		})
	}
	for _, item := range project.Items {
		versions := project.VersionsForItem(item.ID)
		tree := ChainTreeV2{DataItemID: item.ID, Nodes: []VersionV2{}}
		for _, version := range versions {
			createTxn, ok := project.TxnByID(version.CreateTxn)
			if !ok {
				return nil, domain.NewError(domain.EInputBadCreateRef, "cannot encode unknown create txn").
					With("version_id", version.ID).With("txn_id", version.CreateTxn)
			}
			node := VersionV2{
				VersionID:     version.ID,
				CreatedBy:     encodeRefV2(createTxn),
				ContentDigest: version.Digest,
				WriteAlgebra:  version.WriteGen,
				ChainOrder:    version.ChainPos,
			}
			if version.DeleteTxn != nil {
				deleteTxn, ok := project.TxnByID(*version.DeleteTxn)
				if !ok {
					return nil, domain.NewError(domain.EInputBadDeleteRef, "cannot encode unknown delete txn").
						With("version_id", version.ID).With("txn_id", *version.DeleteTxn)
				}
				ref := encodeRefV2(deleteTxn)
				node.RemovedBy = &ref
			}
			tree.Nodes = append(tree.Nodes, node)
		}
		if len(tree.Nodes) > 0 {
			// VersionsForItem sorts ascending ChainPos; chain head is newest.
			tree.HeadVersionID = tree.Nodes[len(tree.Nodes)-1].VersionID
		}
		doc.VersionChainTrees = append(doc.VersionChainTrees, tree)
	}
	for _, op := range project.Ops {
		out := OpV2{
			SequenceNo:      op.Index,
			OperationID:     op.ID,
			OperationType:   string(op.Kind),
			DataItemID:      op.ItemID,
			AtMilliseconds:  op.AtMS,
			NewVersionID:    op.VersionID,
			ContentDigest:   op.Digest,
			WriteAlgebra:    op.WriteGen,
			GCMaintenanceID: op.GCStepID,
		}
		if op.TxnID != domain.NoTxn {
			txn, ok := project.TxnByID(op.TxnID)
			if !ok {
				return nil, domain.NewError(domain.EInputInvalidRef, "cannot encode op unknown txn").
					With("op_id", op.ID).With("txn_id", op.TxnID)
			}
			out.Transaction = encodeRefV2(txn)
		}
		doc.OperationLog = append(doc.OperationLog, out)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func loadV2(data []byte) (*domain.Project, error) {
	var doc EnvelopeV2
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, domain.NewError(domain.ESchemaUnknown, "v2 JSON cannot be decoded").
			At("schema[v2]").With("decode_error", err.Error())
	}
	if doc.NumberingBasis != "RELATIVE_COMMIT_SEQUENCE_WITH_ALIAS_FALLBACK" {
		return nil, domain.NewError(domain.ESchemaBadCombination, "unsupported v2 numbering basis").
			At("numberingBasis").With("value", doc.NumberingBasis)
	}
	if doc.TimeUnit != "MILLISECOND" || doc.ChainOrganization != "GROUPED_TREE" {
		return nil, domain.NewError(domain.ESchemaBadCombination, "unsupported v2 feature combination").
			With("timeUnit", doc.TimeUnit).With("chainOrganization", doc.ChainOrganization)
	}

	project := &domain.Project{
		SchemaVersion: domain.ProjectModelVersion,
		SourceSchema:  domain.SchemaV2,
		ProjectID:     doc.ProjectID,
		BatchID:       doc.BatchID,
		Seed:          doc.Seed,
		Generator:     doc.GeneratorVersion,
		Rules: domain.Rules{
			CommitTieRule: domain.CommitTieRule(doc.DeclarationRules.CommitTieAdjudication),
			ConflictRule:  domain.ConflictRule(doc.DeclarationRules.WriteConflictPolicy),
			TimeUnit:      "millisecond",
		},
	}
	for _, item := range doc.DataItems {
		project.Items = append(project.Items, domain.Item{ID: item.DataItemID, Description: item.Description})
	}
	for i, txn := range doc.TransactionDirectory {
		if txn.TxnAlias == "" {
			return nil, domain.NewError(domain.EInputInvalidRef, "transaction alias is required").
				At(fmt.Sprintf("transactionDirectory[%d].txnAlias", i))
		}
		// Directory order is the declared global monotonic order. Keeping the
		// numeric id equal to the slot makes basis conversion explicit and
		// deterministic for local samples.
		globalID := uint64(i + 1)
		project.Txns = append(project.Txns, domain.Txn{
			ID:                globalID,
			Ref:               txn.TxnAlias,
			StartMS:           txn.StartMilliseconds,
			CommitMS:          txn.CommitMilliseconds,
			RollbackMS:        txn.RollbackMilliseconds,
			Isolation:         domain.Isolation(txn.IsolationLevel),
			RelativeCommitSeq: txn.CommitSequence,
		})
	}
	resolver := newRefResolver(project)

	for treeIndex, tree := range doc.VersionChainTrees {
		if tree.DataItemID == "" {
			return nil, domain.NewError(domain.EInputInvalidRef, "chain tree requires dataItemId").
				At(fmt.Sprintf("versionChainTrees[%d].dataItemId", treeIndex))
		}
		for nodeIndex, node := range tree.Nodes {
			path := fmt.Sprintf("versionChainTrees[%d].nodes[%d]", treeIndex, nodeIndex)
			createTxn, err := resolver.resolve(node.CreatedBy, path+".createdBy")
			if err != nil {
				return nil, err
			}
			version := domain.Version{
				ID:        node.VersionID,
				ItemID:    tree.DataItemID,
				CreateTxn: createTxn,
				Digest:    node.ContentDigest,
				WriteGen:  node.WriteAlgebra,
				ChainPos:  node.ChainOrder,
			}
			if node.RemovedBy != nil {
				deleteTxn, err := resolver.resolve(*node.RemovedBy, path+".removedBy")
				if err != nil {
					return nil, err
				}
				version.DeleteTxn = &deleteTxn
			}
			project.Versions = append(project.Versions, version)
		}
	}

	for opIndex, op := range doc.OperationLog {
		canonical := domain.Op{
			Index:     op.SequenceNo,
			ID:        op.OperationID,
			Kind:      domain.OpKind(op.OperationType),
			ItemID:    op.DataItemID,
			AtMS:      op.AtMilliseconds,
			VersionID: op.NewVersionID,
			Digest:    op.ContentDigest,
			WriteGen:  op.WriteAlgebra,
			GCStepID:  op.GCMaintenanceID,
		}
		if op.Transaction != (TxnRefV2{}) {
			txnID, err := resolver.resolve(op.Transaction, fmt.Sprintf("operationLog[%d].transaction", opIndex))
			if err != nil {
				return nil, err
			}
			canonical.TxnID = txnID
		}
		project.Ops = append(project.Ops, canonical)
	}
	if err := project.Validate(); err != nil {
		return nil, err
	}
	return project, nil
}
