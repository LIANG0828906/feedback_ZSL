package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"proxydiag/bizerr"
	"proxydiag/domain"
)

const (
	SchemaV1 = "proxy.project/v1"
	SchemaV2 = "proxy.project/v2"
	SchemaV3 = "proxy.project/v3"
)

func LoadFile(path string) (*domain.Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.IO, "read project", err).With("path", path)
	}
	p, err := Load(b)
	if err != nil {
		if e, ok := bizerr.As(err); ok {
			return nil, e.With("path", path)
		}
		return nil, err
	}
	return p, nil
}

func Load(data []byte) (*domain.Project, error) {
	var envelope struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, bizerr.Wrap(bizerr.SchemaFieldCombo, "invalid project JSON", err)
	}
	var p *domain.Project
	var err error
	switch envelope.Schema {
	case SchemaV1:
		p, err = loadV1(data)
	case SchemaV2:
		p, err = loadV2(data)
	case SchemaV3:
		p, err = loadV3(data)
	case "":
		return nil, bizerr.New(bizerr.SchemaUnknown, "schema discriminator is required").
			With("supported", SchemaV1+","+SchemaV2+","+SchemaV3)
	default:
		return nil, bizerr.New(bizerr.SchemaUnknown, "unrecognized project schema").
			With("schema", envelope.Schema).
			With("supported", SchemaV1+","+SchemaV2+","+SchemaV3)
	}
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func decodeStrict(data []byte, dst any, schema string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		msg := err.Error()
		code := bizerr.SchemaFieldCombo
		if strings.Contains(msg, "unknown field") {
			code = bizerr.SchemaFieldCombo
		}
		return bizerr.Wrap(code, "schema field combination is not valid for this version", err).
			With("schema", schema).With("detail", msg)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return bizerr.New(bizerr.SchemaFieldCombo, "trailing content after project JSON").
			With("schema", schema)
	}
	return nil
}

func required(schema, field, value string) error {
	if strings.TrimSpace(value) == "" {
		return bizerr.New(bizerr.SchemaRequired, "required field is missing").
			With("schema", schema).With("field", field)
	}
	return nil
}

func convertHeaders(in []domain.Header) []domain.Header {
	if in == nil {
		return []domain.Header{}
	}
	out := make([]domain.Header, len(in))
	copy(out, in)
	return out
}

func scaleCeil(value, multiplier, divisor int64, what string) (int64, error) {
	if value < 0 || multiplier <= 0 || divisor <= 0 {
		return 0, bizerr.New(bizerr.InvalidArgument, "invalid scale conversion").With(what, fmt.Sprintf("%d/%d/%d", value, multiplier, divisor))
	}
	if value == 0 {
		return 0, nil
	}
	if value > (int64(^uint64(0)>>1)-divisor+1)/multiplier {
		return 0, bizerr.New(bizerr.FixedOverflow, "scale conversion overflow").With(what, "overflow")
	}
	return (value*multiplier + divisor - 1) / divisor, nil
}

var _ = convertHeaders
var _ = scaleCeil

func rawError(schema, context string, err error) error {
	return bizerr.Wrap(bizerr.SchemaFieldCombo, "cannot decode schema value", err).
		With("schema", schema).With("context", context)
}
