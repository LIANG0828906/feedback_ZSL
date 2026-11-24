package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	"fixedsampler/internal/loader"
	"fixedsampler/internal/model"
	"fixedsampler/internal/verifier"
)

func main() {
	if err := run(); err != nil {
		var pe *model.PipelineError
		var dl *model.DiffLocation
		if errors.As(err, &pe) {
			b, _ := json.MarshalIndent(pe, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else if errors.As(err, &dl) {
			b, _ := json.MarshalIndent(dl, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintf(os.Stderr, "VERIFY_ERROR: %v\n", err)
		}
		os.Exit(1)
	}
}

func run() error {
	projectPath := flag.String("project", "samples/blocks420/project.json", "project/config manifest path")
	truthPath := flag.String("truth", "samples/blocks420/truth.json", "truth manifest path")
	dbPath := flag.String("db", "checkpoints.db", "persisted checkpoint SQLite DB")
	expectedTruthFP := flag.String("truth-fingerprint", "", "optional pinned truth fingerprint")
	flag.Parse()

	p, err := loader.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	truth, err := loader.LoadTruth(*truthPath)
	if err != nil {
		return err
	}
	if *expectedTruthFP != "" && truth.Fingerprint != *expectedTruthFP {
		return &model.PipelineError{Code: model.CodeDiff, Message: "pinned truth fingerprint does not match manifest"}
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	rs, err := db.Query(`
SELECT artifact_json, config_fingerprint, param_version, algo_version, content_hashes_json
FROM chunk_checkpoints WHERE project_id=? ORDER BY chunk_index`, p.ProjectID)
	if err != nil {
		return err
	}
	defer rs.Close()
	rows := map[int]verifier.PersistedRow{}
	for rs.Next() {
		var artifactJSON, configFP, paramVersion, algoVersion, contentJSON string
		if err := rs.Scan(&artifactJSON, &configFP, &paramVersion, &algoVersion, &contentJSON); err != nil {
			return err
		}
		var chunk model.ChunkArtifact
		if err := json.Unmarshal([]byte(artifactJSON), &chunk); err != nil {
			return err
		}
		var hashes []string
		if err := json.Unmarshal([]byte(contentJSON), &hashes); err != nil {
			return err
		}
		rows[chunk.ChunkIndex] = verifier.PersistedRow{
			Artifact: chunk, ConfigFP: configFP, ParamVersion: paramVersion,
			AlgoVersion: algoVersion, ContentHashes: hashes,
		}
	}
	if err := rs.Err(); err != nil {
		return err
	}

	result, err := verifier.Verify(p, truth, rows, loader.ConfigFingerprint(p))
	if result != nil {
		b, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(b))
	}
	if err != nil {
		return err
	}
	fmt.Println(result.String())
	return nil
}
