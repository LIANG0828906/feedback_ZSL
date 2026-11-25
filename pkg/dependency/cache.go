package dependency

import (
	"context"
	"encoding/json"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

const PackageCacheScope = "lip_package"

type CacheStore interface {
	PutCache(ctx context.Context, scope, key string, payload []byte, recipeVersion, algorithmVersion string) error
	GetCache(ctx context.Context, scope, key, recipeVersion, algorithmVersion string) ([]byte, bool, error)
}

// PersistPackages stores only package nodes in the dependency graph. The cache
// key is the node's full upstream-derived key.
func PersistPackages(ctx context.Context, store CacheStore, graph *Graph, recipe domain.Recipe) error {
	if store == nil || graph == nil {
		return &domain.Error{Code: domain.ErrStage, Field: "cache", Reason: "store and graph are required"}
	}
	ids := make([]string, 0, len(graph.Nodes))
	for id := range graph.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := graph.Nodes[id]
		if node.Kind != NodePackage {
			continue
		}
		if err := store.PutCache(ctx, PackageCacheScope, node.CacheKey, node.Payload, recipe.Version, recipe.AlgorithmVersion); err != nil {
			return err
		}
	}
	return nil
}

// LoadReusablePackages returns persisted packages whose node cache key survived
// invalidation. Any package explicitly invalidated is never read from cache.
func LoadReusablePackages(ctx context.Context, store CacheStore, graph *Graph, report Invalidation, recipe domain.Recipe) (map[string]domain.LipPackage, []string, error) {
	out := make(map[string]domain.LipPackage)
	invalid := make(map[string]struct{}, len(report.InvalidatedPackages))
	for _, id := range report.InvalidatedPackages {
		invalid[id] = struct{}{}
	}
	var missing []string
	for _, packageID := range report.ReusedPackages {
		if _, bad := invalid[packageID]; bad {
			continue
		}
		var node *Node
		for _, candidate := range graph.Nodes {
			if candidate.Kind == NodePackage && candidate.PackageID == packageID {
				node = candidate
				break
			}
		}
		if node == nil {
			missing = append(missing, packageID)
			continue
		}
		payload, ok, err := store.GetCache(ctx, PackageCacheScope, node.CacheKey, recipe.Version, recipe.AlgorithmVersion)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			missing = append(missing, packageID)
			continue
		}
		var pkg domain.LipPackage
		if err := json.Unmarshal(payload, &pkg); err != nil {
			return nil, nil, &domain.Error{
				Code: domain.ErrDecode, PackageID: packageID, Field: "package_cache",
				Reason: "decode persisted package", Cause: err,
			}
		}
		out[packageID] = pkg
	}
	sort.Strings(missing)
	return out, missing, nil
}
