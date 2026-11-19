package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"driftconverge/internal/model"

	"gopkg.in/yaml.v3"
)

type fileSnapshot struct {
	path string
	data []byte
}

func ParseSnapshot(cluster, branch, commit string, files map[string][]byte) (*model.Snapshot, error) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	snapshot := &model.Snapshot{
		Cluster:    cluster,
		Branch:     branch,
		Commit:     commit,
		Namespaces: make(map[string]bool),
	}
	seenDeployments := make(map[string]string)
	ordered := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		ordered = append(ordered, fileSnapshot{path: path, data: files[path]})
	}

	policyCount := 0
	for _, file := range ordered {
		decoder := yaml.NewDecoder(bytes.NewReader(file.data))
		for docIndex := 1; ; docIndex++ {
			var doc any
			err := decoder.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			fallbackKey := fmt.Sprintf("%s#doc-%d", file.path, docIndex)
			if err != nil {
				return nil, manifestError(cluster, branch, fallbackKey, "", "invalid YAML: "+err.Error())
			}
			if doc == nil {
				continue
			}
			obj, ok := doc.(map[string]any)
			if !ok {
				return nil, manifestError(cluster, branch, fallbackKey, "", "manifest document must be an object")
			}
			kind, err := requiredString(obj, "kind")
			if err != nil {
				return nil, manifestError(cluster, branch, fallbackKey, "kind", err.Error())
			}
			apiVersion, err := requiredString(obj, "apiVersion")
			if err != nil {
				return nil, manifestError(cluster, branch, documentKey(cluster, obj, fallbackKey), "apiVersion", err.Error())
			}
			switch kind {
			case "Namespace":
				if apiVersion != "v1" {
					return nil, manifestError(cluster, branch, documentKey(cluster, obj, fallbackKey), "apiVersion", "Namespace must use apiVersion v1")
				}
				name, parseErr := parseNamespace(obj)
				if parseErr != nil {
					return nil, manifestError(cluster, branch, fallbackKey, "metadata.name", parseErr.Error())
				}
				if snapshot.Namespaces[name] {
					return nil, manifestError(cluster, branch, fallbackKey, "metadata.name", "duplicate Namespace declaration "+name)
				}
				snapshot.Namespaces[name] = true
			case "Deployment":
				if apiVersion != "apps/v1" {
					return nil, manifestError(cluster, branch, documentKey(cluster, obj, fallbackKey), "apiVersion", "Deployment must use apiVersion apps/v1")
				}
				deployment, parseErr := parseDeployment(cluster, obj)
				if parseErr != nil {
					key := deploymentResourceKey(cluster, obj)
					if key == "" {
						key = fallbackKey
					}
					var be *model.BusinessError
					if errors.As(parseErr, &be) {
						be.SourceBranch = branch
						return nil, be
					}
					return nil, manifestError(cluster, branch, key, "", parseErr.Error())
				}
				if previous, found := seenDeployments[deployment.ResourceKey]; found {
					return nil, manifestError(
						cluster,
						branch,
						deployment.ResourceKey,
						"",
						fmt.Sprintf("duplicate resource key (also declared in %s)", previous),
					)
				}
				seenDeployments[deployment.ResourceKey] = file.path
				snapshot.Deployments = append(snapshot.Deployments, deployment)
			case "BranchPolicy":
				policy, parseErr := parsePolicy(obj, file.path)
				if parseErr != nil {
					return nil, manifestError(cluster, branch, fallbackKey, "", "invalid branch policy: "+parseErr.Error())
				}
				policyCount++
				if policyCount > 1 {
					return nil, manifestError(cluster, branch, fallbackKey, "", "duplicate BranchPolicy document")
				}
				snapshot.Policy = policy
				snapshot.PolicyPresent = true
			default:
				// Non-Deployment workload kinds are outside the convergence scope.
			}
		}
	}

	for i := range snapshot.Deployments {
		dep := &snapshot.Deployments[i]
		if !snapshot.Namespaces[dep.Namespace] {
			return nil, manifestError(
				cluster,
				branch,
				dep.ResourceKey,
				"metadata.namespace",
				"namespace "+dep.Namespace+" is not declared in this snapshot",
			)
		}
	}
	sort.Slice(snapshot.Deployments, func(i, j int) bool {
		return snapshot.Deployments[i].ResourceKey < snapshot.Deployments[j].ResourceKey
	})
	if len(snapshot.Deployments) == 0 {
		return nil, manifestError(cluster, branch, "<snapshot>", "", "snapshot contains no Deployment resources")
	}
	return snapshot, nil
}

func parseNamespace(obj map[string]any) (string, error) {
	metadata, err := requiredMap(obj, "metadata")
	if err != nil {
		return "", err
	}
	name, err := requiredString(metadata, "name")
	if err != nil {
		return "", fmt.Errorf("metadata.name is required")
	}
	return name, nil
}

func parseDeployment(cluster string, obj map[string]any) (model.DesiredDeployment, error) {
	key := deploymentResourceKey(cluster, obj)
	metadata, err := requiredMap(obj, "metadata")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "metadata", err.Error())
	}
	name, err := requiredString(metadata, "name")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "metadata.name", err.Error())
	}
	namespace, err := requiredString(metadata, "namespace")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "metadata.namespace", err.Error())
	}
	key = model.ResourceKey(cluster, namespace, name)
	spec, err := requiredMap(obj, "spec")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec", err.Error())
	}
	replicas, err := requiredInt(spec, "replicas")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.replicas", err.Error())
	}
	if replicas < 0 || replicas > 1000000 {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.replicas", "replicas must be between 0 and 1000000")
	}
	template, err := requiredMap(spec, "template")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.template", err.Error())
	}
	templateMetadata, err := requiredMap(template, "metadata")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.template.metadata", err.Error())
	}
	if _, err := requiredMap(templateMetadata, "labels"); err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.template.metadata.labels", err.Error())
	}
	podSpec, err := requiredMap(template, "spec")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.template.spec", err.Error())
	}
	containersRaw, ok := podSpec["containers"]
	if !ok {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.template.spec.containers", "field is required")
	}
	containersSlice, ok := containersRaw.([]any)
	if !ok || len(containersSlice) == 0 {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.template.spec.containers", "must be a non-empty array")
	}

	containers := make([]model.DesiredContainer, 0, len(containersSlice))
	containerNames := make(map[string]bool)
	for index, raw := range containersSlice {
		field := fmt.Sprintf("spec.template.spec.containers[%d]", index)
		containerObj, ok := raw.(map[string]any)
		if !ok {
			return model.DesiredDeployment{}, manifestError(cluster, "", key, field, "must be an object")
		}
		containerName, err := requiredString(containerObj, "name")
		if err != nil {
			return model.DesiredDeployment{}, manifestError(cluster, "", key, field+".name", err.Error())
		}
		if containerNames[containerName] {
			return model.DesiredDeployment{}, manifestError(cluster, "", key, field+".name", "duplicate container name "+containerName)
		}
		containerNames[containerName] = true
		image, err := requiredString(containerObj, "image")
		if err != nil {
			return model.DesiredDeployment{}, manifestError(cluster, "", key, field+".image", err.Error())
		}

		env := make([]model.EnvVar, 0)
		if envRaw, exists := containerObj["env"]; exists {
			envSlice, ok := envRaw.([]any)
			if !ok {
				return model.DesiredDeployment{}, manifestError(cluster, "", key, field+".env", "must be an array")
			}
			envNames := make(map[string]bool)
			for envIndex, entryRaw := range envSlice {
				envField := fmt.Sprintf("%s.env[%d]", field, envIndex)
				entry, ok := entryRaw.(map[string]any)
				if !ok {
					return model.DesiredDeployment{}, manifestError(cluster, "", key, envField, "must be an object")
				}
				envName, err := requiredString(entry, "name")
				if err != nil {
					return model.DesiredDeployment{}, manifestError(cluster, "", key, envField+".name", err.Error())
				}
				if envNames[envName] {
					return model.DesiredDeployment{}, manifestError(cluster, "", key, envField+".name", "duplicate environment key "+envName)
				}
				envNames[envName] = true
				if _, hasValueFrom := entry["valueFrom"]; hasValueFrom {
					return model.DesiredDeployment{}, manifestError(
						cluster,
						"",
						key,
						envField,
						"valueFrom is not supported for deterministic env comparison; declare a literal value",
					)
				}
				value, hasValue := entry["value"]
				if !hasValue {
					return model.DesiredDeployment{}, manifestError(cluster, "", key, envField+".value", "field is required")
				}
				valueString, ok := scalarString(value)
				if !ok {
					return model.DesiredDeployment{}, manifestError(cluster, "", key, envField+".value", "must be a scalar string")
				}
				env = append(env, model.EnvVar{Name: envName, Value: valueString})
			}
			sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })
		}
		containers = append(containers, model.DesiredContainer{
			Name:  containerName,
			Image: image,
			Env:   env,
		})
	}

	selector, err := requiredMap(spec, "selector")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.selector", err.Error())
	}
	matchLabels, err := requiredMap(selector, "matchLabels")
	if err != nil {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.selector.matchLabels", err.Error())
	}
	if len(matchLabels) == 0 {
		return model.DesiredDeployment{}, manifestError(cluster, "", key, "spec.selector.matchLabels", "must not be empty")
	}

	return model.DesiredDeployment{
		ResourceKey: key,
		Cluster:     cluster,
		Namespace:   namespace,
		Name:        name,
		Replicas:    int32(replicas),
		Containers:  containers,
		Raw:         obj,
	}, nil
}

func parsePolicy(obj map[string]any, sourceFile string) (model.BranchPolicy, error) {
	spec, err := requiredMap(obj, "spec")
	if err != nil {
		return model.BranchPolicy{}, err
	}
	policy := model.BranchPolicy{SourceFile: sourceFile}
	if raw, ok := spec["prodReplicaCeiling"]; ok {
		value, ok := scalarInt(raw)
		if !ok || value < 0 {
			return policy, fmt.Errorf("spec.prodReplicaCeiling must be a non-negative integer")
		}
		policy.ProdReplicaCeiling = int(value)
		policy.ReplicaCeilingSet = true
	}
	policy.ForbiddenImageTags, err = stringListAny(spec, "prodForbiddenImageTags", "devOnlyImageTags")
	if err != nil {
		return policy, err
	}
	policy.ForbiddenEnvKeys, err = stringListAny(spec, "prodForbiddenEnvKeys", "devOnlyEnvKeys")
	if err != nil {
		return policy, err
	}
	sort.Strings(policy.ForbiddenImageTags)
	sort.Strings(policy.ForbiddenEnvKeys)
	return policy, nil
}

func stringListAny(spec map[string]any, names ...string) ([]string, error) {
	for _, name := range names {
		raw, ok := spec[name]
		if !ok {
			continue
		}
		values, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("spec.%s must be an array of strings", name)
		}
		out := make([]string, 0, len(values))
		for index, item := range values {
			text, ok := scalarString(item)
			if !ok || text == "" {
				return nil, fmt.Errorf("spec.%s[%d] must be a non-empty string", name, index)
			}
			out = append(out, text)
		}
		return out, nil
	}
	return nil, nil
}

func deploymentResourceKey(cluster string, obj map[string]any) string {
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := scalarString(metadata["name"])
	namespace, _ := scalarString(metadata["namespace"])
	if name == "" {
		return ""
	}
	if namespace == "" {
		namespace = "<namespace-missing>"
	}
	return model.ResourceKey(cluster, namespace, name)
}

func documentKey(cluster string, obj map[string]any, fallback string) string {
	if key := deploymentResourceKey(cluster, obj); key != "" {
		return key
	}
	if metadata, ok := obj["metadata"].(map[string]any); ok {
		if name, ok := scalarString(metadata["name"]); ok && name != "" {
			return cluster + "/<namespace-missing>/" + name
		}
	}
	return fallback
}

func manifestError(cluster, branch, key, field, message string) *model.BusinessError {
	return &model.BusinessError{
		Code:         model.CodeManifest,
		Message:      message,
		ResourceKey:  key,
		Field:        field,
		SourceBranch: branch,
	}
}

func requiredMap(obj map[string]any, key string) (map[string]any, error) {
	raw, ok := obj[key]
	if !ok {
		return nil, fmt.Errorf("%s is required", key)
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return value, nil
}

func requiredString(obj map[string]any, key string) (string, error) {
	raw, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("%s is required", key)
	}
	value, ok := scalarString(raw)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return value, nil
}

func requiredInt(obj map[string]any, key string) (int64, error) {
	raw, ok := obj[key]
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	value, ok := scalarInt(raw)
	if !ok {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return value, nil
}

func scalarString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case int:
		return fmt.Sprintf("%d", typed), true
	case int64:
		return fmt.Sprintf("%d", typed), true
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", typed), "0"), "."), true
	case bool:
		if typed {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

func scalarInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}
