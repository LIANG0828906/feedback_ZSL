package main

import (
	"encoding/json"
	"os"
)

func jsonEncodeStdout(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
