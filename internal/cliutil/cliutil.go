// Package cliutil provides the shared JSON error envelope and integer exit
// code mapping used by every command entry point on Windows and POSIX.
package cliutil

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"vqa/internal/errs"
)

// Fail prints a structured error envelope to stderr and exits with the stable
// integer code for the business error.
func Fail(err error) {
	code := errs.CodeOf(err)
	if code == "" {
		code = "E_INTERNAL"
	}
	envelope := map[string]any{
		"code":    code,
		"message": err.Error(),
	}
	if be, ok := err.(*errs.Error); ok && len(be.Fields) > 0 {
		envelope["fields"] = be.Fields
	}
	payload, _ := json.Marshal(envelope)
	fmt.Fprintln(os.Stderr, string(payload))
	os.Exit(errs.ExitCode(code))
}

// EmitJSON writes a JSON object to stdout.
func EmitJSON(v any) {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		Fail(err)
	}
	fmt.Println(string(payload))
}

// RequiredFlag fails with E_SCHEMA when a mandatory string flag is empty.
func RequiredFlag(name, value string) {
	if value == "" {
		Fail(errs.New(errs.ESchema, "missing required flag").With("flag", name))
	}
}

// Parse is a small wrapper around flag.Parse for symmetry across commands.
func Parse() { flag.Parse() }
