package anomaly

import (
	"credrot/internal/bizerr"
	"credrot/internal/model"
)

type Kind string

const (
	DuplicateCredentialRef Kind = "duplicate_credential_ref"
	NegativeTTL            Kind = "negative_ttl"
	BatchLimitZero         Kind = "batch_limit_zero"
	SubscriptionCycle      Kind = "subscription_cycle"
	MissingCredential      Kind = "missing_credential"
	DuplicateCredential    Kind = "duplicate_credential"
)

func Inject(source *model.Project, kind Kind) (*model.Project, error) {
	if source == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "source project is nil", nil)
	}
	out := source.Clone()
	switch kind {
	case DuplicateCredentialRef:
		if len(out.Subscriptions) == 0 || len(out.Subscriptions[0].Refs) == 0 {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project has no credential reference to duplicate", nil)
		}
		out.Subscriptions[0].Refs = append(out.Subscriptions[0].Refs, out.Subscriptions[0].Refs[0])
	case NegativeTTL:
		if len(out.Credentials) == 0 {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project has no credential to edit", nil)
		}
		out.Credentials[0].TTLMillis = -1000
	case BatchLimitZero:
		out.Reload.MaxPerBatch = 0
	case SubscriptionCycle:
		if len(out.Subscriptions) == 0 {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project has no subscription to edit", nil)
		}
		root := out.Subscriptions[0].ID
		// Find a node transitively below root and make root depend on it.
		children := make(map[string][]string)
		for _, sub := range out.Subscriptions {
			for _, dep := range sub.DependsOn {
				children[dep] = append(children[dep], sub.ID)
			}
		}
		descendant := ""
		queue := append([]string(nil), children[root]...)
		for len(queue) > 0 && descendant == "" {
			id := queue[0]
			queue = queue[1:]
			for _, child := range children[id] {
				descendant = child
				queue = append(queue, children[child]...)
				break
			}
			if descendant == "" && len(queue) > 0 {
				descendant = queue[len(queue)-1]
			}
		}
		if descendant == "" {
			descendant = root
		}
		out.Subscriptions[0].DependsOn = append(out.Subscriptions[0].DependsOn, descendant)
	case MissingCredential:
		if len(out.Subscriptions) == 0 || len(out.Subscriptions[0].Refs) == 0 {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project has no credential reference to break", nil)
		}
		out.Subscriptions[0].Refs[0].CredentialID = "credential-that-does-not-exist"
	case DuplicateCredential:
		if len(out.Credentials) == 0 {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project has no credential to duplicate", nil)
		}
		out.Credentials = append(out.Credentials, out.Credentials[0])
	default:
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "unknown anomaly injection", map[string]any{"kind": kind})
	}
	out.Sort()
	return out, nil
}
