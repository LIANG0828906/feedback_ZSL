package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"sre-timeline/internal/model"
)

type hashSnapshot struct {
	ID       string          `json:"id"`
	Revision string          `json:"revision"`
	Seed     int64           `json:"seed"`
	Window   model.Window    `json:"window"`
	Policy   model.Policy    `json:"policy"`
	Services []model.Service `json:"services"`
	Edges    []model.Edge    `json:"edges"`
	Events   []model.Event   `json:"events"`
}

// ContentHash hashes normalized engineering content while ignoring the source
// schema variant and optional truth representation. TickMS is representation
// metadata for V2 and is therefore excluded by clearing it in the copy.
func ContentHash(p *model.Project) string {
	events := p.SortedEvents()
	for i := range events {
		events[i].Raw = nil
	}
	snapshot := hashSnapshot{
		ID: p.ID, Revision: p.Revision, Seed: p.Seed,
		Window:   model.Window{StartMS: p.Window.StartMS, EndMS: p.Window.EndMS},
		Policy:   p.Policy,
		Services: p.Services,
		Edges:    p.EdgesSorted(),
		Events:   events,
	}
	data, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func serviceFingerprint(p *model.Project, serviceID string) string {
	svc, _ := p.Service(serviceID)
	var events []model.Event
	for _, ev := range p.SortedEvents() {
		if ev.ServiceID == serviceID {
			ev.Raw = nil
			events = append(events, ev)
		}
	}
	data, _ := json.Marshal(struct {
		Service model.Service `json:"service"`
		Events  []model.Event `json:"events"`
		Window  model.Window  `json:"window"`
		MergeMS int64         `json:"merge_ms"`
	}{svc, events, model.Window{StartMS: p.Window.StartMS, EndMS: p.Window.EndMS}, p.Policy.MergeGapMS})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func CloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
