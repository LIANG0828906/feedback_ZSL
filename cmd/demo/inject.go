package main

import (
	"encoding/json"
	"fmt"

	"coldkernel/internal/errcode"
	"coldkernel/internal/gen"
	"coldkernel/internal/loader"
)

// runInjection demonstrates that every malformed input yields a stable
// E_xxx code while the source snapshot stays byte-identical.
func runInjection(rawV1 []byte) error {
	type probe struct {
		name string
		mut  func(*gen.SnapshotV1)
		want errcode.Code
	}
	probes := []probe{
		{"negative concurrency", func(s *gen.SnapshotV1) { s.Functions[0].ConcurrencyLimit = -1 }, errcode.ENegConcurrency},
		{"reserved > limit", func(s *gen.SnapshotV1) { s.Functions[0].ReservedSlots = 99 }, errcode.EReservedExceeds},
		{"warm pool overrun", func(s *gen.SnapshotV1) {
			for i := int64(0); i <= s.Functions[0].ConcurrencyLimit; i++ {
				s.WarmPool = append(s.WarmPool, gen.V1WarmEntry{FunctionID: s.Functions[0].FunctionID, InstanceID: "x"})
			}
		}, errcode.EPoolOutOfRange},
		{"tick out of order", func(s *gen.SnapshotV1) { s.Requests[len(s.Requests)-1].ArriveTick = 0 }, errcode.ETickOrder},
		{"unknown function", func(s *gen.SnapshotV1) { s.Requests[0].FunctionID = "ghost" }, errcode.EUnknownFunction},
		{"bad budget", func(s *gen.SnapshotV1) {
			bad := int64(9999)
			s.Functions[0].ColdBudgetPermille = &bad
		}, errcode.EBadBudget},
		{"unknown unit", func(s *gen.SnapshotV1) { s.TickUnit = "nope" }, errcode.EUnitUnknown},
	}
	fmt.Println("== Exception injection (source never mutated) ==")
	for _, pr := range probes {
		var snap gen.SnapshotV1
		if err := json.Unmarshal(rawV1, &snap); err != nil {
			return err
		}
		pr.mut(&snap)
		raw, err := json.Marshal(&snap)
		if err != nil {
			return err
		}
		if _, err := loader.Load(raw); err == nil {
			return fmt.Errorf("%s: expected error %s", pr.name, pr.want)
		} else {
			code, e := errcode.From(err)
			if code != pr.want {
				return fmt.Errorf("%s: got %s want %s", pr.name, code, pr.want)
			}
			fmt.Printf("  %-24s -> %s (%s)\n", pr.name, code, e.Message)
		}
	}
	var generic map[string]any
	if err := json.Unmarshal(rawV1, &generic); err != nil {
		return err
	}
	generic["schema_version"] = "coldkernel/v9"
	raw, _ := json.Marshal(generic)
	if _, err := loader.Load(raw); err == nil {
		return fmt.Errorf("unknown schema: expected error")
	} else {
		code, _ := errcode.From(err)
		fmt.Printf("  %-24s -> %s\n", "unknown schema version", code)
	}
	fmt.Println("all injection probes returned stable codes; source snapshot unchanged")
	return nil
}
