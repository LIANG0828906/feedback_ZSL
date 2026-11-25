package samplegen

import (
	"encoding/json"

	"github.com/liang/hapticfeedback/internal/haptic/branches"
)

const (
	InjectCorrupt     = "corrupt"
	InjectOrder       = "order"
	InjectFlag        = "flag"
	InjectMissingFlag = "missing_flag"
	InjectBadJSON     = "bad_json"
)

func InjectedBranchBytes(kind string, branch int) ([]byte, error) {
	stream := CanonicalStream()
	exported, err := branches.Export(stream, branch)
	if err != nil {
		return nil, err
	}
	if kind == InjectBadJSON {
		return append(append([]byte{}, exported.Data...), []byte("\n{\"broken\":")...), nil
	}

	var file map[string]any
	if err := json.Unmarshal(exported.Data, &file); err != nil {
		return nil, err
	}
	frames := file["frames"].([]any)

	switch kind {
	case InjectCorrupt:
		frame := frames[2].(map[string]any)
		frame["p"] = "corrupt-integer"
	case InjectOrder:
		frame := frames[2].(map[string]any)
		frame["at"] = 0
	case InjectFlag, InjectMissingFlag:
		var target map[string]any
		for _, item := range frames {
			candidate := item.(map[string]any)
			if int(candidate["frame_no"].(float64)) == 6 {
				target = candidate
				break
			}
		}
		if kind == InjectMissingFlag {
			delete(target, "flag")
		} else if branch == 2 {
			target["flag"] = 1
		} else {
			target["flag"] = 99
		}
	}
	return json.MarshalIndent(file, "", "  ")
}
