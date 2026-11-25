package loader

import (
	"encoding/json"
	"strconv"
	"strings"

	"battlekernel/internal/errs"
)

func decodeDocument(data []byte, target any) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		if typeErr, ok := err.(*json.UnmarshalTypeError); ok {
			field := typeErr.Field
			if field == "" {
				field = "$"
			}
			return nil, errs.Wrap(errs.ESchema, field, err, "field has invalid type")
		}
		return nil, errs.Wrap(errs.EDecode, "$", err, "engineering document is not valid JSON")
	}
	if err := json.Unmarshal(data, target); err != nil {
		if typeErr, ok := err.(*json.UnmarshalTypeError); ok {
			field := typeErr.Field
			if field == "" {
				field = "$"
			}
			return nil, errs.Wrap(errs.ESchema, field, err, "field has invalid type")
		}
		return nil, errs.Wrap(errs.EDecode, "$", err, "engineering document could not be decoded")
	}
	return raw, nil
}

func requirePath(raw any, path string) error {
	current := raw
	tokens := tokenizePath(path)
	for _, token := range tokens {
		switch token.kind {
		case tokenField:
			m, ok := current.(map[string]any)
			if !ok {
				return errs.New(errs.ESchema, path, "required field is missing or not an object")
			}
			value, ok := m[token.name]
			if !ok || value == nil {
				return errs.New(errs.ESchema, path, "required field is missing")
			}
			current = value
		case tokenIndex:
			arr, ok := current.([]any)
			if !ok {
				return errs.New(errs.ESchema, path, "required array is missing")
			}
			if token.index < 0 || token.index >= len(arr) || arr[token.index] == nil {
				return errs.New(errs.ESchema, path, "required index is missing")
			}
			current = arr[token.index]
		}
	}
	if s, ok := current.(string); ok && strings.TrimSpace(s) == "" {
		return errs.New(errs.ESchema, path, "required string is empty")
	}
	return nil
}

type pathToken struct {
	kind  byte
	name  string
	index int
}

const (
	tokenField byte = iota
	tokenIndex
)

func tokenizePath(path string) []pathToken {
	var tokens []pathToken
	for _, part := range strings.Split(path, ".") {
		for {
			open := strings.IndexByte(part, '[')
			if open < 0 {
				if part != "" {
					tokens = append(tokens, pathToken{kind: tokenField, name: part})
				}
				break
			}
			if open > 0 {
				tokens = append(tokens, pathToken{kind: tokenField, name: part[:open]})
			}
			close := strings.IndexByte(part[open:], ']')
			if close < 0 {
				tokens = append(tokens, pathToken{kind: tokenField, name: part[open:]})
				break
			}
			indexText := part[open+1 : open+close]
			idx, _ := strconv.Atoi(indexText)
			tokens = append(tokens, pathToken{kind: tokenIndex, index: idx})
			part = part[open+close+1:]
		}
	}
	return tokens
}

func requireMany(raw any, paths []string) error {
	for _, path := range paths {
		if err := requirePath(raw, path); err != nil {
			return err
		}
	}
	return nil
}
