package plancache

import (
	"fmt"
	"regexp"
	"strconv"
)

var placeholderPattern = regexp.MustCompile(`\$([0-9]+)`)

// renumberTemplate applies offset to numeric $n placeholders. The offset is
// added when rendering global numbering and subtracted while loading V1.
func renumberTemplate(template string, offset, paramCount int, path string) (string, error) {
	var outErr error
	out := placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		sub := placeholderPattern.FindStringSubmatch(match)
		n, err := strconv.Atoi(sub[1])
		if err != nil {
			outErr = withContext(newError(CodeParamIndexOutOfRange, path, "invalid placeholder number %q", match), "detail", err.Error())
			return match
		}
		renumbered := n + offset
		zeroBased := renumbered - 1
		if paramCount > 0 && (zeroBased < 0 || zeroBased >= paramCount+offset) {
			outErr = withContext(newError(CodeParamIndexOutOfRange, path, "placeholder number %s is outside declared parameters", match), "placeholder", n, "offset", offset, "param_count", paramCount)
			return match
		}
		return fmt.Sprintf("$%d", renumbered)
	})
	if outErr != nil {
		return "", outErr
	}
	return out, nil
}

func normalizeGlobalTemplate(template string, base, paramCount int, path string) (string, error) {
	var outErr error
	out := placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		sub := placeholderPattern.FindStringSubmatch(match)
		globalOneBased, err := strconv.Atoi(sub[1])
		if err != nil {
			outErr = withContext(newError(CodeParamIndexOutOfRange, path, "invalid global placeholder %q", match), "detail", err.Error())
			return match
		}
		relativeOneBased := globalOneBased - base
		zeroBased := relativeOneBased - 1
		if zeroBased < 0 || zeroBased >= paramCount {
			outErr = withContext(newError(CodeParamIndexOutOfRange, path, "global placeholder %s maps outside statement parameters", match), "global", globalOneBased, "base", base, "param_count", paramCount)
			return match
		}
		return fmt.Sprintf("$%d", relativeOneBased)
	})
	if outErr != nil {
		return "", outErr
	}
	return out, nil
}
