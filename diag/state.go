package diag

import (
	"encoding/json"
	"os"
	"path/filepath"

	"qkernel/internal/chain"
	"qkernel/internal/errs"
	"qkernel/internal/sched"
	"qkernel/internal/session"
	"qkernel/internal/synth"
)

type State struct {
	Root         string                    `json:"root"`
	Run          *session.RunRecord        `json:"run"`
	Truth        *synth.GroundTruth        `json:"truth"`
	Chain        *chain.Chain              `json:"chain"`
	Reuse        []session.ReuseRecord     `json:"reuse"`
	Invalidation json.RawMessage           `json:"invalidation"`
	UnitStates   []session.UnitStateRecord `json:"unit_states"`
	QueueEvents  []sched.QueueEvent        `json:"queue_events"`
	Metrics      json.RawMessage           `json:"metrics"`
	Attribution  json.RawMessage           `json:"attribution"`
	Verification json.RawMessage           `json:"verification"`
	Images       ImagePaths                `json:"images"`
}

type ImagePaths struct {
	Base    string   `json:"base"`
	Final   string   `json:"final"`
	Steps   []string `json:"steps"`
	Metrics string   `json:"metrics"`
	Attr    string   `json:"attribution"`
}

func BuildState(root, runID string) (*State, error) {
	if runID == "" {
		if b, err := os.ReadFile(filepath.Join(root, "verification.json")); err == nil {
			var pinned struct {
				RunID string `json:"run_id"`
			}
			if json.Unmarshal(b, &pinned) == nil && pinned.RunID != "" {
				runID = pinned.RunID
			}
		}
	}
	store, err := session.OpenStore(root)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	var run *session.RunRecord
	if runID == "" {
		run, err = store.LatestRun()
	} else {
		run, err = store.GetRun(runID)
	}
	if err != nil {
		return nil, err
	}

	truth, err := synth.LoadTruth(filepath.Join(root, "truth", run.FrameID+".json"))
	if err != nil {
		return nil, err
	}
	chainJSON, _, err := store.ChainForVersion(run.ParamVersion)
	if err != nil {
		return nil, err
	}
	var c chain.Chain
	if err := json.Unmarshal([]byte(chainJSON), &c); err != nil {
		return nil, errs.Wrap(errs.ESchema, err, "decode chain")
	}
	units, err := store.ListUnitStates(run.RunID)
	if err != nil {
		return nil, err
	}

	st := &State{Root: root, Run: run, Truth: truth, Chain: &c, UnitStates: units}
	st.Reuse = loadJSON[session.ReuseRecord](filepath.Join(root, "reuse", run.FrameID+".json"))
	st.Invalidation = loadRaw(filepath.Join(root, "invalidation", run.FrameID+".json"))
	st.QueueEvents = loadJSON[sched.QueueEvent](filepath.Join(root, "queue", run.FrameID+".json"))
	st.Metrics = loadRaw(filepath.Join(root, "metrics", run.FrameID+".json"))
	st.Attribution = loadRaw(filepath.Join(root, "attribution", run.FrameID+".json"))
	st.Verification = loadRaw(filepath.Join(root, "verification.json"))
	st.Images = ImagePaths{
		Base:    "/files/base/" + run.FrameID + ".png",
		Final:   "/files/images/" + run.FrameID + "/final.png",
		Metrics: "/files/metrics/" + run.FrameID + ".json",
		Attr:    "/files/attribution/" + run.FrameID + ".json",
	}
	for i, step := range c.Steps {
		st.Images.Steps = append(st.Images.Steps, "/files/images/"+run.FrameID+"/"+stepFileName(i+1, string(step.Type))+".png")
	}
	return st, nil
}

func loadJSON[T any](path string) []T {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v []T
	_ = json.Unmarshal(b, &v)
	return v
}

func loadRaw(path string) json.RawMessage {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

func stepFileName(step int, typ string) string {
	prefix := "step_"
	if step < 10 {
		prefix += "0"
	}
	return prefix + itoa(step) + "_" + typ
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
