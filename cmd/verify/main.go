// verify 是独立复核命令行：仅凭 SQLite 快照与真值指纹重新核对，
// 不经过结算主路径。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"replaykernel/internal/model"
	"replaykernel/internal/verify"
)

func main() {
	dbPath := flag.String("db", "", "检查点 SQLite 路径（只读打开）")
	truthPath := flag.String("truth", "", "真值清单 JSON 路径")
	flag.Parse()
	if *dbPath == "" || *truthPath == "" {
		fmt.Fprintln(os.Stderr, "usage: verify -db <checkpoint.db> -truth <match.truth.json>")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*truthPath)
	if err != nil {
		fatal(err)
	}
	var truth model.MatchTruth
	if err := json.Unmarshal(raw, &truth); err != nil {
		fatal(model.NewError(model.EDecode, "truth file corrupted: %v", err))
	}

	rep, err := verify.Verify(*dbPath, &truth)
	if err != nil {
		fatal(err)
	}
	if rep.OK {
		fmt.Printf("PASS match=%s checked_turns=%d\n", rep.MatchID, rep.Checked)
		return
	}
	f := rep.First
	fmt.Printf("FAIL match=%s first_violation turn=%d unit=%s field=%s expect=%s got=%s\n",
		rep.MatchID, f.TurnNo, f.UnitID, f.Field, f.Expect, f.Got)
	os.Exit(1)
}

func fatal(err error) {
	if ke, ok := model.AsKernelError(err); ok {
		fmt.Fprintln(os.Stderr, ke.Error())
	} else {
		fmt.Fprintln(os.Stderr, err.Error())
	}
	os.Exit(1)
}
