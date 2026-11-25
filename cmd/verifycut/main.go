package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"iconcut/verifier"
)

func main() {
	db := flag.String("db", "checkpoint.db", "SQLite checkpoint path")
	flag.Parse()
	rep, err := verifier.VerifyDB(context.Background(), *db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifycut: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if !rep.OK {
		os.Exit(2)
	}
}
