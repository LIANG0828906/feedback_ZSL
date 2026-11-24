package interop

import (
	"bytes"
	"encoding/json"
	"io"

	"saveinterop/internal/core"
	"saveinterop/internal/generator"
)

type decodedArchive struct {
	Archive generator.Archive
	Hash    string
}

func decodeArchive(data []byte) (decodedArchive, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var archive generator.Archive
	if err := decoder.Decode(&archive); err != nil {
		return decodedArchive{}, &core.Error{
			Code: core.CodeSemantic, Op: "decode-archive",
			Message: "invalid archive JSON: " + err.Error(), Window: -1,
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return decodedArchive{}, &core.Error{
			Code: core.CodeSemantic, Op: "decode-archive",
			Message: "archive has trailing JSON values", Window: -1,
		}
	}
	if archive.Save == nil {
		archive.Save = make(map[string]any)
	}
	return decodedArchive{Archive: archive, Hash: core.HashBytes(data)}, nil
}

func branchRuleError(op, archiveVersion, tableVersion, sourceBranch string) error {
	return &core.Error{
		Code:         core.CodeMapping,
		Op:           op,
		Message:      "mapping rule version mismatch",
		Entry:        "archive.mappingVersion",
		SourceBranch: sourceBranch,
		TargetBranch: "runtime",
		Expected:     tableVersion,
		Actual:       archiveVersion,
		Window:       -1,
	}
}

func semanticError(op, path, branch, role string, measured any, message string) error {
	if message == "" {
		message = "semantic value cannot be determined"
	}
	return &core.Error{
		Code: core.CodeSemantic, Op: op, Message: message,
		FieldPath: path, Branch: branch, Semantic: role, Measured: measured,
		Conflict: role, Window: -1,
	}
}
