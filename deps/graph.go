package deps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lipsync/domain"
)

type NodeKind string

const (
	NodeParameter NodeKind = "parameter"
	NodePacket    NodeKind = "packet"
	NodeFrame     NodeKind = "frame"
)

// Node is one explicit dependency in the arbitration DAG.
type Node struct {
	ID            string                     `json:"id"`
	Kind          NodeKind                   `json:"kind"`
	ChannelID     string                     `json:"channel_id,omitempty"`
	PacketID      string                     `json:"packet_id,omitempty"`
	Frame         int64                      `json:"frame,omitempty"`
	CacheKey      string                     `json:"cache_key"`
	Upstream      []string                   `json:"upstream"`
	ParamsVersion string                     `json:"params_version,omitempty"`
	Result        *domain.FrameChannelResult `json:"result,omitempty"`
}

// Graph is deliberately serializable so unchanged frame payloads can be
// revalidated from a persistent store after an edit.
type Graph struct {
	ContentHash   string                   `json:"content_hash"`
	ParamsVersion string                   `json:"params_version"`
	Nodes         map[string]Node          `json:"nodes"`
	NodeOrder     []string                 `json:"node_order"`
	Result        domain.ArbitrationResult `json:"result"`
}

func Build(project domain.Project, result domain.ArbitrationResult) *Graph {
	graph := &Graph{
		ContentHash: project.ContentHash, Nodes: map[string]Node{}, Result: result,
	}
	recoveryKey := hashParts("recovery", string(project.Recovery.Strategy),
		fmt.Sprint(project.Recovery.RecoveryOffsetFrame), fmt.Sprint(project.Recovery.MinContinuousFrames),
		fmt.Sprint(project.Recovery.AllowAbandon), fmt.Sprint(project.Recovery.WindowFrames))
	orderKey := hashParts(append([]string{"order"}, project.ArbitrationOrder...)...)
	graph.ParamsVersion = hashParts("params", recoveryKey, orderKey, project.ContentHash)
	addNode := func(node Node) {
		graph.Nodes[node.ID] = node
		graph.NodeOrder = append(graph.NodeOrder, node.ID)
	}
	addNode(Node{
		ID: "param:recovery", Kind: NodeParameter, CacheKey: recoveryKey,
		ParamsVersion: graph.ParamsVersion,
	})
	addNode(Node{
		ID: "param:arbitration_order", Kind: NodeParameter, CacheKey: orderKey,
		ParamsVersion: graph.ParamsVersion,
		Upstream:      []string{"param:recovery"},
	})
	packetByID := map[string]domain.Packet{}
	for _, packet := range project.Packets() {
		packetByID[packet.ID] = packet
	}
	packetNodeIDs := map[string]string{}
	for _, packet := range project.Packets() {
		channel, _ := project.ChannelByID(packet.ChannelID)
		slotKey := hashParts("slot", packet.ChannelID, fmt.Sprint(channel.SlotLimit))
		budgetKey := hashParts("budget", packet.ChannelID, fmt.Sprint(channel.FrameBudgetBasisPoints))
		slotID := "param:channel:" + packet.ChannelID + ":slot"
		budgetID := "param:channel:" + packet.ChannelID + ":budget"
		if _, exists := graph.Nodes[slotID]; !exists {
			addNode(Node{
				ID: slotID, Kind: NodeParameter, ChannelID: packet.ChannelID,
				CacheKey: slotKey, ParamsVersion: graph.ParamsVersion,
				Upstream: []string{"param:arbitration_order"},
			})
		}
		if _, exists := graph.Nodes[budgetID]; !exists {
			addNode(Node{
				ID: budgetID, Kind: NodeParameter, ChannelID: packet.ChannelID,
				CacheKey: budgetKey, ParamsVersion: graph.ParamsVersion,
				Upstream: []string{"param:arbitration_order"},
			})
		}
		nodeID := "packet:" + packet.ID
		upstream := []string{slotID, budgetID, "param:recovery", "param:arbitration_order"}
		keyParts := []string{"packet", packet.ContentHash, packet.ID, packet.ChannelID, graph.ParamsVersion}
		keyParts = append(keyParts, upstreamKeys(graph, upstream)...)
		key := hashParts(keyParts...)
		addNode(Node{
			ID: nodeID, Kind: NodePacket, ChannelID: packet.ChannelID,
			PacketID: packet.ID, CacheKey: key, ParamsVersion: graph.ParamsVersion,
			Upstream: upstream,
		})
		packetNodeIDs[packet.ID] = nodeID
	}
	for _, frame := range result.Frames {
		for i := range frame.Channels {
			channelResult := frame.Channels[i]
			packetIDs := uniqueStrings(append(append(append(append([]string{},
				channelResult.Accepted...), channelResult.Delayed...),
				channelResult.Preempted...), channelResult.Resumed...))
			upstream := []string{
				"param:arbitration_order", "param:recovery",
				"param:channel:" + channelResult.ChannelID + ":slot",
				"param:channel:" + channelResult.ChannelID + ":budget",
			}
			for _, id := range packetIDs {
				if packetNode, ok := packetNodeIDs[id]; ok {
					upstream = append(upstream, packetNode)
				}
			}
			upstream = uniqueStrings(upstream)
			sort.Strings(upstream)
			keyParts := []string{"frame", channelResult.ChannelID, fmt.Sprint(frame.Frame), graph.ParamsVersion}
			keyParts = append(keyParts, upstreamKeys(graph, upstream)...)
			key := hashParts(keyParts...)
			payload := channelResult
			addNode(Node{
				ID: frameNodeID(channelResult.ChannelID, frame.Frame), Kind: NodeFrame,
				ChannelID: channelResult.ChannelID, Frame: frame.Frame, CacheKey: key,
				ParamsVersion: graph.ParamsVersion, Upstream: upstream, Result: &payload,
			})
		}
	}
	return graph
}

func frameNodeID(channelID string, frame int64) string {
	return fmt.Sprintf("frame:%s:%d", channelID, frame)
}

func hashParts(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func upstreamKeys(graph *Graph, ids []string) []string {
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		if node, ok := graph.Nodes[id]; ok {
			keys = append(keys, node.CacheKey)
		}
	}
	sort.Strings(keys)
	return keys
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func (g *Graph) MarshalStable() ([]byte, error) {
	return json.MarshalIndent(g, "", "  ")
}

// Store preserves frame payloads across graph edits. Implementations may be
// memory-backed for tests or SQLite-backed for the diagnostic kernel.
type Store interface {
	GetFrame(nodeID string) (DomainFrameEntry, bool)
	PutFrame(entry DomainFrameEntry) error
	DeleteFrame(nodeID string) error
}

type DomainFrameEntry struct {
	NodeID    string                    `json:"node_id"`
	CacheKey  string                    `json:"cache_key"`
	ChannelID string                    `json:"channel_id"`
	Frame     int64                     `json:"frame"`
	Result    domain.FrameChannelResult `json:"result"`
}

type MemoryStore struct {
	entries map[string]DomainFrameEntry
	Hits    int
	Misses  int
	Writes  int
	Deletes int
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]DomainFrameEntry{}}
}

func (s *MemoryStore) GetFrame(nodeID string) (DomainFrameEntry, bool) {
	entry, ok := s.entries[nodeID]
	if ok {
		s.Hits++
	} else {
		s.Misses++
	}
	return entry, ok
}

func (s *MemoryStore) PutFrame(entry DomainFrameEntry) error {
	if s.entries == nil {
		s.entries = map[string]DomainFrameEntry{}
	}
	s.entries[entry.NodeID] = entry
	s.Writes++
	return nil
}

func (s *MemoryStore) DeleteFrame(nodeID string) error {
	delete(s.entries, nodeID)
	s.Deletes++
	return nil
}

func (s *MemoryStore) Len() int { return len(s.entries) }
