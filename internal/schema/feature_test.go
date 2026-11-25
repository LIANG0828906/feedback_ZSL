package schema_test

import (
	"strings"
	"testing"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/generator"
	"mvccworkbench/internal/schema"
)

func TestExternalSchemaFeatureMatrix(t *testing.T) {
	suite, err := generator.DefaultSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Batches["B1-visibility"].Project

	v1, err := schema.Encode(domain.SchemaV1, project)
	if err != nil {
		t.Fatal(err)
	}
	text1 := string(v1)
	for _, token := range []string{`schema_version`, `start_tick`, `"txn_id"`, `"delete_txn": null`, `"versions"`} {
		if !strings.Contains(text1, token) {
			t.Fatalf("v1 missing %s", token)
		}
	}

	v2, err := schema.Encode(domain.SchemaV2, project)
	if err != nil {
		t.Fatal(err)
	}
	text2 := string(v2)
	for _, token := range []string{`schemaVersion`, `startMilliseconds`, `commitSequence`, `versionChainTrees`, `RELATIVE_COMMIT_SEQUENCE`} {
		if !strings.Contains(text2, token) {
			t.Fatalf("v2 missing %s", token)
		}
	}

	v3, err := schema.Encode(domain.SchemaV3, project)
	if err != nil {
		t.Fatal(err)
	}
	text3 := string(v3)
	for _, token := range []string{`schema-version`, `start-ms`, `version-chains`, `"txn-id"`} {
		if !strings.Contains(text3, token) {
			t.Fatalf("v3 missing %s", token)
		}
	}
	if strings.Contains(text3, "delete-txn") || strings.Contains(text3, "deleteTxn") {
		t.Fatalf("v3 must omit explicit delete transaction field")
	}
}

func TestZeroDeleteReferenceNormalizesToUndecided(t *testing.T) {
	doc := []byte(`{
		"schema_version":"mvcc.project/v1",
		"project_id":"p",
		"batch_id":"b",
		"rules":{"commit_tie_rule":"GLOBAL_TXN_ID_ASC","conflict_rule":"FIRST_COMMITTER_WINS"},
		"data_items":[{"item_id":"x"}],
		"transactions":[{"txn_id":1,"txn_ref":"t1","start_tick":1,"isolation_level":"READ_COMMITTED"}],
		"versions":[{"version_id":"v1","item_id":"x","create_txn":1,"delete_txn":0,"content_digest":"d","write_algebra":1,"chain_position":0}],
		"operations":[]
	}`)
	project, err := schema.Load(domain.SchemaV1, doc)
	if err != nil {
		t.Fatal(err)
	}
	if project.Versions[0].DeleteTxn != nil {
		t.Fatalf("delete txn=%v want nil undecided", *project.Versions[0].DeleteTxn)
	}
}
