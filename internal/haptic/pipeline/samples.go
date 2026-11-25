package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/liang/hapticfeedback/internal/haptic/branches"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/samplegen"
)

type SampleSet struct {
	Dir        string
	Capability model.Capability
	Truth      samplegen.Truth
	Streams    map[int]*model.Stream
	Raw        map[int][]byte
}

func GenerateAndLoad(dir string) (*SampleSet, error) {
	if _, err := samplegen.Generate(dir); err != nil {
		return nil, err
	}
	return LoadSampleSet(dir)
}

func LoadSampleSet(dir string) (*SampleSet, error) {
	set := &SampleSet{Dir: dir, Streams: map[int]*model.Stream{}, Raw: map[int][]byte{}}
	capBytes, err := os.ReadFile(filepath.Join(dir, "device_capabilities.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(capBytes, &set.Capability); err != nil {
		return nil, err
	}
	truthBytes, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(truthBytes, &set.Truth); err != nil {
		return nil, err
	}
	for branch := 1; branch <= 3; branch++ {
		raw, err := os.ReadFile(filepath.Join(dir, "branch_"+itoa(branch)+".json"))
		if err != nil {
			return nil, err
		}
		stream, err := branches.Load(raw)
		if err != nil {
			return nil, err
		}
		set.Raw[branch] = raw
		set.Streams[branch] = stream
	}
	return set, nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for v > 0 {
		n--
		b[n] = byte('0' + v%10)
		v /= 10
	}
	return string(b[n:])
}
