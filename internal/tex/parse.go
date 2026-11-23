package tex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func ParseDescriptor(data []byte) (Descriptor, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var descriptor Descriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, ErrPara("descriptor", fmt.Sprintf("cannot decode target descriptor: %v", err))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Descriptor{}, ErrPara("descriptor", "target descriptor contains trailing JSON data")
	}
	descriptor = descriptor.Normalize()
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	return descriptor, nil
}

func LoadDescriptorFile(path string) (Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, ErrPara("descriptor", fmt.Sprintf("cannot read target descriptor: %v", err))
	}
	return ParseDescriptor(data)
}
