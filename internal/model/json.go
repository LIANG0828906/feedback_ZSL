package model

import "encoding/json"

func JSONMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func JSONUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
