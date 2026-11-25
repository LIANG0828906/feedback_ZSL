package probe

import (
	"encoding/json"
	"math"
	"strings"

	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

func CloneScene(input *scene.Scene) (*scene.Scene, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var out scene.Scene
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Engine) EditRadiance(probeID string, radiance scene.Vec3) (*MutationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	record, err := e.recordByIDLocked(probeID)
	if err != nil {
		return nil, err
	}
	if e.roomStates[record.RoomID] == RoomCommitted {
		return nil, versionFrozenError(record.RoomID, "edit probe radiance "+probeID)
	}
	for c := 0; c < 3; c++ {
		if math.IsNaN(radiance[c]) || math.IsInf(radiance[c], 0) {
			return nil, (&diag.Error{
				Code: diag.CodeSchema, ID: probeID, RoomID: record.RoomID,
				Field: "radiance", Message: "radiance channels must be finite",
			})
		}
	}

	for i := range e.scene.Probes {
		if e.scene.Probes[i].ID == probeID {
			e.scene.Probes[i].Radiance = radiance
			break
		}
	}
	record.InputRadiance = radiance
	record.Dirty = true

	changed := []string{RadianceSourceNode(probeID)}
	affected := e.graph.ForwardClosure(changed...)
	for _, nodeID := range affected {
		if strings.HasPrefix(nodeID, "recon:") {
			id := strings.TrimPrefix(nodeID, "recon:")
			if r, ok := e.records[id]; ok {
				r.Dirty = true
			}
		}
	}
	plan := e.planFromAffectedLocked("edit_radiance", probeID, changed, affected)
	e.lastPlan = plan
	return plan, nil
}

func (e *Engine) SetResolution(roomID string, resolution scene.Resolution) (*MutationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	room, roomIndex, ok := e.scene.RoomByID(roomID)
	if !ok {
		return nil, diag.Schema("room_id", "unknown room %q", roomID)
	}
	if e.roomStates[roomID] == RoomCommitted {
		return nil, versionFrozenError(roomID, "change resolution")
	}
	if resolution.X <= 0 || resolution.Y <= 0 || resolution.Z <= 0 {
		return nil, (&diag.Error{
			Code: diag.CodeSchema, RoomID: roomID, RoomIndex: roomIndex,
			Field: "resolution", Message: "resolution x/y/z must be positive",
		})
	}

	for i := range e.scene.Rooms {
		if e.scene.Rooms[i].ID == roomID {
			e.scene.Rooms[i].Resolution = resolution
			room = e.scene.Rooms[i]
			break
		}
	}

	filtered := make([]scene.Probe, 0, len(e.scene.Probes))
	for _, probe := range e.scene.Probes {
		if probe.RoomID != roomID {
			filtered = append(filtered, probe)
		}
	}
	e.scene.Probes = append(filtered, scene.GenerateProbesForRoom(room, e.scene.Seed)...)

	oldRecords := e.records
	if err := e.rebuildTopologyLocked(oldRecords, roomID); err != nil {
		return nil, err
	}

	targetIDs := e.allProbeIDsForRoomLocked(roomID)
	changedRecon := make([]string, 0, len(targetIDs))
	items := make([]InvalidationItem, 0, len(targetIDs))
	for _, id := range targetIDs {
		changedRecon = append(changedRecon, ReconNode(id))
		items = append(items, InvalidationItem{
			NodeID: ReconNode(id), Kind: "probe", RoomID: roomID, ProbeID: id,
			Reason: "resolution_topology_changed",
		})
	}
	affected := e.graph.ForwardClosure(changedRecon...)
	plan := e.planFromAffectedLocked("set_resolution", roomID, changedRecon, affected)
	plan.Invalidation = append(items, plan.Invalidation...)
	e.lastPlan = plan
	return plan, nil
}

func (e *Engine) planFromAffectedLocked(mutation, target string, changed, affected []string) *MutationPlan {
	plan := &MutationPlan{Mutation: mutation, TargetID: target, ChangedNodes: append([]string{}, changed...)}
	for _, nodeID := range affected {
		node, ok := e.graph.Nodes[nodeID]
		item := InvalidationItem{NodeID: nodeID, Reason: reasonForNode(nodeID, node)}
		if ok {
			item.Kind = node.Kind
			item.RoomID = node.RoomID
			if node.Kind == "probe" {
				item.ProbeID = strings.TrimPrefix(nodeID, "recon:")
			}
			if node.Kind == "cross_domain_blend" {
				item.AdjacentRoomID = e.adjacentRoomForBlendLocked(node)
			}
		} else {
			item.Kind = nodeKindFromID(nodeID)
		}
		plan.Invalidation = append(plan.Invalidation, item)
	}
	return plan
}

func reasonForNode(nodeID string, node Node) string {
	switch node.Kind {
	case "probe":
		return "probe_reconstruction_dependency_changed"
	case "cross_domain_blend":
		return "cross_domain_blend_corner_invalidated"
	case "interpolation":
		return "trilinear_query_corner_invalidated"
	case "pixel":
		return "screen_pixel_upstream_interpolation_invalidated"
	default:
		if strings.HasPrefix(nodeID, "recon:") {
			return "probe_reconstruction_dependency_changed"
		}
		return "dependency_invalidated"
	}
}

func nodeKindFromID(id string) string {
	switch {
	case strings.HasPrefix(id, "recon:"):
		return "probe"
	case strings.HasPrefix(id, "blend:"):
		return "cross_domain_blend"
	case strings.HasPrefix(id, "interp:"):
		return "interpolation"
	case strings.HasPrefix(id, "pixel:"):
		return "pixel"
	default:
		return "node"
	}
}

func (e *Engine) adjacentRoomForBlendLocked(node Node) string {
	boundaryID := node.Label
	for _, boundary := range e.scene.Boundaries {
		if boundary.ID != boundaryID {
			continue
		}
		if node.RoomID == boundary.RoomA {
			return boundary.RoomB
		}
		if node.RoomID == boundary.RoomB {
			return boundary.RoomA
		}
	}
	return ""
}
