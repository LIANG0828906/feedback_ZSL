package compare

import (
	"fmt"
	"sort"

	"driftconverge/internal/model"
)

func Deployments(expected []model.DesiredDeployment, actual []model.DeploymentState) []model.Difference {
	actualByName := make(map[string]model.DeploymentState, len(actual))
	for _, state := range actual {
		actualByName[state.Name] = state
	}
	out := make([]model.Difference, 0)
	for _, desired := range expected {
		state, found := actualByName[desired.Name]
		if !found {
			out = append(out, model.Difference{
				ResourceKey: desired.ResourceKey,
				Field:       "exists",
				Expected:    "present",
				Actual:      "missing",
				Resolution:  model.ResolutionUnresolved,
			})
			out = append(out, model.Difference{
				ResourceKey: desired.ResourceKey,
				Field:       "replicas",
				Expected:    desired.Replicas,
				Actual:      nil,
				Resolution:  model.ResolutionUnresolved,
			})
			for _, container := range desired.Containers {
				out = append(out, model.Difference{
					ResourceKey: desired.ResourceKey,
					Field:       "image",
					Component:   container.Name,
					Expected:    container.Image,
					Actual:      nil,
					Resolution:  model.ResolutionUnresolved,
				})
				for _, env := range container.Env {
					out = append(out, model.Difference{
						ResourceKey: desired.ResourceKey,
						Field:       "env",
						Component:   container.Name,
						EnvKey:      env.Name,
						Expected:    env.Value,
						Actual:      nil,
						Resolution:  model.ResolutionUnresolved,
					})
				}
			}
			continue
		}
		if desired.Replicas != state.Replicas {
			out = append(out, model.Difference{
				ResourceKey: desired.ResourceKey,
				Field:       "replicas",
				Expected:    desired.Replicas,
				Actual:      state.Replicas,
				Resolution:  model.ResolutionUnresolved,
			})
		}
		actualContainers := make(map[string]model.ContainerState, len(state.Containers))
		for _, container := range state.Containers {
			actualContainers[container.Name] = container
		}
		for _, desiredContainer := range desired.Containers {
			actualContainer, exists := actualContainers[desiredContainer.Name]
			if !exists {
				out = append(out, model.Difference{
					ResourceKey: desired.ResourceKey,
					Field:       "image",
					Component:   desiredContainer.Name,
					Expected:    desiredContainer.Image,
					Actual:      nil,
					Resolution:  model.ResolutionUnresolved,
				})
				for _, env := range desiredContainer.Env {
					out = append(out, model.Difference{
						ResourceKey: desired.ResourceKey,
						Field:       "env",
						Component:   desiredContainer.Name,
						EnvKey:      env.Name,
						Expected:    env.Value,
						Actual:      nil,
						Resolution:  model.ResolutionUnresolved,
					})
				}
				continue
			}
			if desiredContainer.Image != actualContainer.Image {
				out = append(out, model.Difference{
					ResourceKey: desired.ResourceKey,
					Field:       "image",
					Component:   desiredContainer.Name,
					Expected:    desiredContainer.Image,
					Actual:      actualContainer.Image,
					Resolution:  model.ResolutionUnresolved,
				})
			}
			expectedEnv := make(map[string]string, len(desiredContainer.Env))
			for _, env := range desiredContainer.Env {
				expectedEnv[env.Name] = env.Value
				actualValue, envFound := actualContainer.Env[env.Name]
				if !envFound {
					out = append(out, model.Difference{
						ResourceKey: desired.ResourceKey,
						Field:       "env",
						Component:   desiredContainer.Name,
						EnvKey:      env.Name,
						Expected:    env.Value,
						Actual:      nil,
						Resolution:  model.ResolutionUnresolved,
					})
					continue
				}
				if env.Value != actualValue {
					out = append(out, model.Difference{
						ResourceKey: desired.ResourceKey,
						Field:       "env",
						Component:   desiredContainer.Name,
						EnvKey:      env.Name,
						Expected:    env.Value,
						Actual:      actualValue,
						Resolution:  model.ResolutionUnresolved,
					})
				}
			}
			extraKeys := make([]string, 0)
			for key := range actualContainer.Env {
				if _, expected := expectedEnv[key]; !expected {
					extraKeys = append(extraKeys, key)
				}
			}
			sort.Strings(extraKeys)
			for _, key := range extraKeys {
				out = append(out, model.Difference{
					ResourceKey: desired.ResourceKey,
					Field:       "env",
					Component:   desiredContainer.Name,
					EnvKey:      key,
					Expected:    nil,
					Actual:      actualContainer.Env[key],
					Resolution:  model.ResolutionUnresolved,
				})
			}
		}
	}
	model.SortDifferences(out)
	return out
}

func ApplyResolutions(original, remaining []model.Difference, blockedResources map[string]bool) {
	remainingKeys := make(map[string]bool, len(remaining))
	for _, item := range remaining {
		remainingKeys[item.SortKey()] = true
	}
	for index := range original {
		item := &original[index]
		if blockedResources[item.ResourceKey] {
			item.Resolution = model.ResolutionBlocked
			if item.Reason == "" {
				item.Reason = "workload did not reach an admissible stable state before timeout"
			}
			continue
		}
		if remainingKeys[item.SortKey()] {
			item.Resolution = model.ResolutionUnresolved
			if item.Reason == "" {
				item.Reason = "field still differs after apply"
			}
			continue
		}
		item.Resolution = model.ResolutionConverged
	}
	model.SortDifferences(original)
}

func ResourceKeys(differences []model.Difference) []string {
	set := make(map[string]bool)
	for _, item := range differences {
		set[item.ResourceKey] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func DifferencesForResource(differences []model.Difference, resourceKey string) []model.Difference {
	out := make([]model.Difference, 0)
	for _, item := range differences {
		if item.ResourceKey == resourceKey {
			out = append(out, item)
		}
	}
	return out
}

func FormatDiff(item model.Difference) string {
	switch item.Field {
	case "env":
		return fmt.Sprintf("%s env %s", item.Component, item.EnvKey)
	case "image":
		return fmt.Sprintf("%s image", item.Component)
	default:
		return item.Field
	}
}
