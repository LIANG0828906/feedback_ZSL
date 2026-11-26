// Command verify independently audits checkpoints and compares run paths.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"acousticfield/internal/audit"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "audit":
		auditCmd(os.Args[2:])
	case "compare":
		compareCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown verify command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`verify audit <checkpoint-dir>
verify compare <full-dir> <realtime-dir> <recovery-dir> [more-dirs...]`)
}

func auditCmd(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "audit requires exactly one checkpoint directory")
		os.Exit(2)
	}
	result, err := audit.AuditDir(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit error: %v\n", err)
		os.Exit(2)
	}
	writeJSON(result)
	if !result.OK {
		if result.First != nil {
			fmt.Fprintf(os.Stderr, "FIRST_MISMATCH %s\n", result.First.String())
		}
		os.Exit(1)
	}
	fmt.Printf("AUDIT_OK dir=%s chunks=%d findings=0\n", result.Dir, len(result.ChunkOrder))
}

func compareCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "compare requires at least two report directories")
		os.Exit(2)
	}
	cmp, err := audit.CompareReportDirs(args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compare error: %v\n", err)
		os.Exit(2)
	}
	writeJSON(cmp)
	if !cmp.OK {
		if cmp.First != nil {
			fmt.Fprintf(os.Stderr, "FIRST_MISMATCH chunk=%s band=%d field=%s expected=%v actual=%v\n",
				cmp.First.ChunkID, cmp.First.BandID, cmp.First.Field, cmp.First.Expected, cmp.First.Actual)
		}
		os.Exit(1)
	}
	fmt.Printf("COMPARE_OK dirs=%v\n", args)
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
