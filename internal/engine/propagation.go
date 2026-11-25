package engine

import (
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/model"
	"credrot/internal/project"
)

type ReloadArrival struct {
	SubscriptionID string   `json:"subscription_id"`
	Depth          int      `json:"depth"`
	Batch          int64    `json:"batch"`
	Path           []string `json:"path"`
}

type SuspendedCredential struct {
	CredentialID    string          `json:"credential_id"`
	Status          string          `json:"status"`
	SourceVersion   uint64          `json:"source_version"`
	TargetVersion   uint64          `json:"target_version"`
	BaseReloadBatch int64           `json:"base_reload_batch"`
	Arrivals        []ReloadArrival `json:"arrivals"`
	ArrivalSet      []string        `json:"arrival_set"`
	SourceReason    string          `json:"source_reason"`
}

type PropagationResult struct {
	Suspended []SuspendedCredential `json:"suspended"`
}

func Propagate(p *model.Project) (*PropagationResult, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if err := project.Validate(p); err != nil {
		return nil, err
	}
	ctx := newPropagationContext(p)
	result := &PropagationResult{Suspended: make([]SuspendedCredential, 0, len(p.Rotations))}
	for _, rotation := range p.Rotations {
		suspended, err := ctx.one(rotation)
		if err != nil {
			return nil, err
		}
		result.Suspended = append(result.Suspended, suspended)
	}
	sort.Slice(result.Suspended, func(i, j int) bool {
		return result.Suspended[i].CredentialID < result.Suspended[j].CredentialID
	})
	return result, nil
}

type propagationContext struct {
	creds              map[string]model.Credential
	subs               map[string]model.Subscription
	active             map[string]bool
	dependents         map[string][]string
	directByCredential map[string][]string
}

func newPropagationContext(p *model.Project) *propagationContext {
	creds := p.CredentialByID()
	subs := p.SubscriptionByID()
	active := make(map[string]bool, len(p.Subscriptions))
	dependents := make(map[string][]string, len(p.Subscriptions))
	for _, sub := range p.Subscriptions {
		active[sub.ID] = sub.Active
		for _, parent := range sub.DependsOn {
			dependents[parent] = append(dependents[parent], sub.ID)
		}
	}
	for parent := range dependents {
		sort.Strings(dependents[parent])
	}

	directByCredential := make(map[string][]string)
	for _, sub := range p.Subscriptions {
		if !sub.Active {
			continue
		}
		for _, ref := range sub.Refs {
			c := creds[ref.CredentialID]
			if ref.ExpectedVersion == 0 || ref.ExpectedVersion == c.Version {
				directByCredential[ref.CredentialID] = append(directByCredential[ref.CredentialID], sub.ID)
			}
		}
	}
	for id := range directByCredential {
		sort.Strings(directByCredential[id])
	}
	return &propagationContext{
		creds:              creds,
		subs:               subs,
		active:             active,
		dependents:         dependents,
		directByCredential: directByCredential,
	}
}

func (ctx *propagationContext) one(rotation model.Rotation) (SuspendedCredential, error) {
	target, ok := ctx.creds[rotation.CredentialID]
	if !ok {
		return SuspendedCredential{}, bizerr.New(bizerr.CodeMissingCredential, "rotation target is missing", map[string]any{
			"credential_id": rotation.CredentialID,
		})
	}
	direct := ctx.directByCredential[rotation.CredentialID]
	type node struct {
		depth int
		path  []string
	}
	reached := make(map[string]node, len(direct))
	queue := make([]string, 0, len(direct))
	for _, id := range direct {
		if _, exists := reached[id]; exists {
			continue
		}
		reached[id] = node{depth: 0, path: []string{id}}
		queue = append(queue, id)
	}
	// A parent can be reached after its child in sorted order. Repeated
	// relaxation until stable handles DAGs while cycle detection prevents
	// unbounded growth.
	for {
		changed := false
		sort.Strings(queue)
		for _, parent := range queue {
			parentNode := reached[parent]
			for _, child := range ctx.dependents[parent] {
				if !ctx.active[child] {
					continue
				}
				candidateDepth := parentNode.depth + 1
				candidatePath := append(append([]string(nil), parentNode.path...), child)
				current, exists := reached[child]
				if !exists || candidateDepth > current.depth || (candidateDepth == current.depth && pathLess(candidatePath, current.path)) {
					reached[child] = node{depth: candidateDepth, path: candidatePath}
					queue = append(queue, child)
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	arrivals := make([]ReloadArrival, 0, len(reached))
	for id, node := range reached {
		if !ctx.active[id] {
			continue
		}
		_, known := ctx.subs[id]
		if !known {
			return SuspendedCredential{}, bizerr.New(bizerr.CodeMissingSubscription, "propagation reached an unknown subscription", map[string]any{
				"subscription_id": id,
				"credential_id":   rotation.CredentialID,
			})
		}
		arrivals = append(arrivals, ReloadArrival{
			SubscriptionID: id,
			Depth:          node.depth,
			Batch:          target.ReloadBatch + int64(node.depth),
			Path:           append([]string(nil), node.path...),
		})
	}
	sort.Slice(arrivals, func(i, j int) bool {
		if arrivals[i].Batch != arrivals[j].Batch {
			return arrivals[i].Batch < arrivals[j].Batch
		}
		return arrivals[i].SubscriptionID < arrivals[j].SubscriptionID
	})
	set := make([]string, 0, len(arrivals))
	for _, arrival := range arrivals {
		set = append(set, arrival.SubscriptionID)
	}
	return SuspendedCredential{
		CredentialID:    rotation.CredentialID,
		Status:          "suspended",
		SourceVersion:   rotation.FromVersion,
		TargetVersion:   rotation.ToVersion,
		BaseReloadBatch: target.ReloadBatch,
		Arrivals:        arrivals,
		ArrivalSet:      set,
		SourceReason:    "rotation:" + rotation.CredentialID,
	}, nil
}

func pathLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}
