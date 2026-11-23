package tex

import "testing"

func TestParseDescriptorStrictFields(t *testing.T) {
	valid := []byte(`{
		"id":"parse-ok",
		"source_layout":"rgb",
		"target_layout":"rgba",
		"target_bit_depth":8,
		"permutation":[0,1,2],
		"fill":"repeat_alpha"
	}`)
	descriptor, err := ParseDescriptor(valid)
	if err != nil {
		t.Fatalf("ParseDescriptor: %v", err)
	}
	if descriptor.ID != "parse-ok" || descriptor.Version() == "" {
		t.Fatalf("unexpected descriptor: %+v", descriptor)
	}
	unknown := []byte(`{
		"source_layout":"rgb",
		"target_layout":"rgba",
		"target_bit_depth":8,
		"permutation":[0,1,2],
		"fill":"repeat_alpha",
		"surprise":true
	}`)
	_, err = ParseDescriptor(unknown)
	texErr := requireErrorCode(t, err, CodePara)
	if texErr.Field != "descriptor" {
		t.Fatalf("field=%q, want descriptor", texErr.Field)
	}
	_, err = ParseDescriptor([]byte(`{"source_layout":"rgb"}`))
	texErr = requireErrorCode(t, err, CodePara)
	if texErr.Field == "" {
		t.Fatalf("missing-field error must identify a field")
	}
}
