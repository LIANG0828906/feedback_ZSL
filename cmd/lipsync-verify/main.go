package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"lipsync/domain"
	"lipsync/truth"
	"lipsync/verify"
)

func main() {
	truthPath := flag.String("truth", "artifacts/generated/truth.json", "sealed truth manifest")
	dbList := flag.String("db", "artifacts/checkpoints/realtime.db", "comma-separated SQLite checkpoint files")
	flag.Parse()

	ctx := context.Background()
	m, err := truth.LoadFile(*truthPath)
	if err != nil {
		fail(err)
	}
	var paths []string
	for _, p := range strings.Split(*dbList, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		fail(&domain.Error{Code: domain.CodeParam, Field: "db", Message: "no database supplied"})
	}
	var reports []*verify.Report
	for _, path := range paths {
		r, err := verify.VerifyDB(ctx, path, m)
		if err != nil {
			fail(err)
		}
		reports = append(reports, r)
	}
	out := map[string]any{"databases": reports}
	if len(paths) > 1 {
		comparison, err := verify.CompareDBs(ctx, paths...)
		if err != nil {
			fail(err)
		}
		out["comparison"] = comparison
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func fail(err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		b, _ := json.MarshalIndent(de, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
