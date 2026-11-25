package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"audiobus/domain"
	aerr "audiobus/errs"
	"audiobus/internal/loader"
	"audiobus/internal/pipeline"
)

type faultCase struct {
	Case     string `json:"case"`
	Code     string `json:"code"`
	Location string `json:"location"`
}

func faultsCmd(args []string) {
	fs := flag.NewFlagSet("faults", flag.ExitOnError)
	sampleDir := fs.String("samples", "artifacts/sample", "sample directory")
	_ = fs.Parse(args)
	set, err := loader.LoadSampleSet(*sampleDir)
	fatal(err)

	cases := []faultCase{}
	add := func(name string, err error) {
		ae, ok := err.(*aerr.Error)
		if !ok {
			fatal(err)
		}
		location := fmt.Sprintf("path=%s branch=%s channel=%d window=%d packet=%d frame=%d event=%d", ae.Path, ae.Branch, ae.Channel, ae.Window, ae.Packet, ae.Frame, ae.EventID)
		cases = append(cases, faultCase{Case: name, Code: string(ae.Code), Location: location})
	}

	_, _, err = loader.LoadBranchBytes("alpha", []byte(`{"branch":`), &set.Manifest)
	add("corrupt_event_stream", err)

	raw, err := os.ReadFile(filepath.Join(*sampleDir, "branch_alpha.json"))
	fatal(err)
	var bf domain.BranchFile
	fatal(json.Unmarshal(raw, &bf))
	*bf.Events[1].Ts = 1
	mutated, err := json.Marshal(bf)
	fatal(err)
	_, _, err = loader.LoadBranchBytes("alpha", mutated, &set.Manifest)
	add("frame_timing_inversion", err)

	err = loader.ValidateFlags(loader.CanonicalEvents(set), &set.Manifest, 1)
	add("conditional_flag_conflict", err)

	engine := pipeline.NewEngine(set.Manifest, loader.CanonicalEvents(set), nil)
	engine.Fault = &pipeline.Fault{Window: 0, Channel: 0, Seq: 1}
	_, err = engine.RunRealtime(context.Background())
	add("single_packet_stage_failure", err)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	fatal(enc.Encode(cases))
}
