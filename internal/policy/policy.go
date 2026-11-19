package policy

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"driftconverge/internal/model"
)

type EffectivePolicy struct {
	ReplicaCeiling     int
	ForbiddenImageTags map[string]bool
	ForbiddenEnvKeys   map[string]bool
	SourceBranch       string
}

func Build(dev, prod *model.Snapshot) EffectivePolicy {
	effective := EffectivePolicy{
		ForbiddenImageTags: make(map[string]bool),
		ForbiddenEnvKeys:   make(map[string]bool),
		SourceBranch:       dev.Branch,
	}

	devTags := imageTags(dev)
	prodTags := imageTags(prod)
	for tag := range devTags {
		if !prodTags[tag] {
			effective.ForbiddenImageTags[tag] = true
		}
	}
	devEnvKeys := envKeys(dev)
	prodEnvKeys := envKeys(prod)
	for key := range devEnvKeys {
		if !prodEnvKeys[key] {
			effective.ForbiddenEnvKeys[key] = true
		}
	}
	for _, dep := range dev.Deployments {
		if int(dep.Replicas) > effective.ReplicaCeiling {
			effective.ReplicaCeiling = int(dep.Replicas)
		}
	}

	if dev.PolicyPresent {
		for _, tag := range dev.Policy.ForbiddenImageTags {
			effective.ForbiddenImageTags[tag] = true
		}
		for _, key := range dev.Policy.ForbiddenEnvKeys {
			effective.ForbiddenEnvKeys[key] = true
		}
		if dev.Policy.ReplicaCeilingSet {
			effective.ReplicaCeiling = dev.Policy.ProdReplicaCeiling
		}
	}
	return effective
}

func CheckProd(prod *model.Snapshot, effective EffectivePolicy) []model.PolicyBlock {
	blocks := make([]model.PolicyBlock, 0)
	for _, dep := range prod.Deployments {
		for _, container := range dep.Containers {
			tag := ImageTag(container.Image)
			if matchesAny(tag, effective.ForbiddenImageTags) {
				blocks = append(blocks, model.PolicyBlock{
					ResourceKey:      dep.ResourceKey,
					Field:            "image:" + container.Name,
					Value:            container.Image,
					SourceBranch:     prod.Branch,
					DeclaredInBranch: effective.SourceBranch,
					Reason:           "image tag " + tag + " is declared dev-only and is forbidden in prod",
				})
			}
			for _, env := range container.Env {
				if effective.ForbiddenEnvKeys[env.Name] {
					blocks = append(blocks, model.PolicyBlock{
						ResourceKey:      dep.ResourceKey,
						Field:            "env:" + env.Name,
						Value:            env.Value,
						SourceBranch:     prod.Branch,
						DeclaredInBranch: effective.SourceBranch,
						Reason:           "environment key " + env.Name + " is declared dev-only and is forbidden in prod",
					})
				}
			}
		}
		if effective.ReplicaCeiling >= 0 && int(dep.Replicas) > effective.ReplicaCeiling {
			blocks = append(blocks, model.PolicyBlock{
				ResourceKey:      dep.ResourceKey,
				Field:            "replicas",
				Value:            dep.Replicas,
				SourceBranch:     prod.Branch,
				DeclaredInBranch: effective.SourceBranch,
				Reason: fmt.Sprintf(
					"replicas %d exceeds dev-declared prod ceiling %d",
					dep.Replicas,
					effective.ReplicaCeiling,
				),
			})
		}
	}
	model.SortPolicyBlocks(blocks)
	return blocks
}

func ImageTag(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[lastColon+1:]
	}
	return "latest"
}

func imageTags(snapshot *model.Snapshot) map[string]bool {
	out := make(map[string]bool)
	if snapshot == nil {
		return out
	}
	for _, dep := range snapshot.Deployments {
		for _, container := range dep.Containers {
			if tag := ImageTag(container.Image); tag != "" {
				out[tag] = true
			}
		}
	}
	return out
}

func envKeys(snapshot *model.Snapshot) map[string]bool {
	out := make(map[string]bool)
	if snapshot == nil {
		return out
	}
	for _, dep := range snapshot.Deployments {
		for _, container := range dep.Containers {
			for _, env := range container.Env {
				out[env.Name] = true
			}
		}
	}
	return out
}

func matchesAny(tag string, patterns map[string]bool) bool {
	if patterns[tag] {
		return true
	}
	for pattern := range patterns {
		if strings.ContainsAny(pattern, "*?[") {
			if matched, err := path.Match(pattern, tag); err == nil && matched {
				return true
			}
		}
	}
	return false
}

func (e EffectivePolicy) SortedTags() []string {
	out := make([]string, 0, len(e.ForbiddenImageTags))
	for item := range e.ForbiddenImageTags {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func (e EffectivePolicy) SortedEnvKeys() []string {
	out := make([]string, 0, len(e.ForbiddenEnvKeys))
	for item := range e.ForbiddenEnvKeys {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}
