package projectio

import (
	"encoding/json"
	"strconv"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/session"
)

const (
	SchemaV1Wire = "mirrorcast.project/v1"
	SchemaV2Wire = "mirrorcast.project/v2"
	SchemaV3Wire = "mirrorcast.project/v3"
	TruthWire    = "mirrorcast.truth/v1"
)

type wireEvent struct {
	EventID     string                `json:"event_id"`
	LogicalTick int64                 `json:"logical_tick"`
	EventType   string                `json:"event_type"`
	ScreenID    string                `json:"screen_id,omitempty"`
	BudgetKbps  int64                 `json:"budget_kbps,omitempty"`
	Topology    *wireTopology         `json:"topology,omitempty"`
	Update      *session.ScreenUpdate `json:"update,omitempty"`
}

type wireTopology struct {
	TopologyVersion int64      `json:"topology_version"`
	Hosts           []wireHost `json:"hosts"`
}

type wireHost struct {
	HostID    string   `json:"host_id"`
	ScreenIDs []string `json:"screen_ids"`
}

func eventToWire(event session.Event) wireEvent {
	out := wireEvent{
		EventID:     event.ID,
		LogicalTick: event.Tick,
		EventType:   string(event.Type),
		ScreenID:    event.ScreenID,
		BudgetKbps:  event.Budget,
		Update:      event.Update,
	}
	if event.Topology != nil {
		topology := &wireTopology{TopologyVersion: event.Topology.Version}
		for _, host := range event.Topology.Hosts {
			topology.Hosts = append(topology.Hosts, wireHost{HostID: host.ID, ScreenIDs: append([]string(nil), host.ScreenIDs...)})
		}
		out.Topology = topology
	}
	return out
}

func eventsFromWire(events []wireEvent) ([]session.Event, error) {
	out := make([]session.Event, 0, len(events))
	for i, raw := range events {
		event := session.Event{
			ID:       raw.EventID,
			Tick:     raw.LogicalTick,
			Type:     session.EventType(raw.EventType),
			ScreenID: raw.ScreenID,
			Budget:   raw.BudgetKbps,
			Update:   raw.Update,
		}
		if raw.Topology != nil {
			event.Topology = &session.Topology{Version: raw.Topology.TopologyVersion}
			for _, host := range raw.Topology.Hosts {
				event.Topology.Hosts = append(event.Topology.Hosts, session.TopologyHost{
					ID:        host.HostID,
					ScreenIDs: append([]string(nil), host.ScreenIDs...),
				})
			}
		}
		if event.ID == "" {
			event.ID = "event-" + jsonNumber(i)
		}
		out = append(out, event)
	}
	return out, nil
}

func jsonNumber(i int) string {
	return strconv.Itoa(i)
}

func topologyToWire(topology *session.Topology) *wireTopology {
	if topology == nil {
		return nil
	}
	out := &wireTopology{TopologyVersion: topology.Version}
	for _, host := range topology.Hosts {
		out.Hosts = append(out.Hosts, wireHost{HostID: host.ID, ScreenIDs: append([]string(nil), host.ScreenIDs...)})
	}
	return out
}

func decodeJSON(data []byte, target any) error {
	if err := json.Unmarshal(data, target); err != nil {
		var typeErr *json.UnmarshalTypeError
		if AsTypeError(err, &typeErr) {
			return bizerror.New(bizerror.EFieldType, "JSON field has an incompatible type: "+err.Error()).WithPath(typeErr.Field)
		}
		return bizerror.New("E_JSON_INVALID", "invalid JSON project: "+err.Error())
	}
	return nil
}

func AsTypeError(err error, target **json.UnmarshalTypeError) bool {
	for err != nil {
		if te, ok := err.(*json.UnmarshalTypeError); ok {
			*target = te
			return true
		}
		type wrapper interface{ Unwrap() error }
		w, ok := err.(wrapper)
		if !ok {
			return false
		}
		err = w.Unwrap()
	}
	return false
}
