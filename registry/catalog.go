package registry

import "sort"

func normalizedDisplayWeight(ppm int64, scale WeightScale) int64 {
	denominator := scale.Denominator()
	if denominator == 0 {
		return 0
	}
	return ppm * denominator / 1_000_000
}

func buildCatalog(project *Project, states map[string]*runtimeInstance, service Service, atMS, tick int64, displayScale WeightScale, limit int) Catalog {
	if displayScale == "" {
		displayScale = service.DefaultWeightScale
	}
	catalog := Catalog{
		ServiceID:  service.ID,
		AtMS:       atMS,
		Candidates: []Candidate{},
		Excluded:   []ExcludedInstance{},
	}
	for _, baseInstance := range project.Instances {
		if baseInstance.ServiceID != service.ID {
			continue
		}
		instance := EffectiveInstance(project, baseInstance, tick)
		runtime := states[instance.ID]
		if runtime == nil {
			continue
		}
		state := runtime.state
		weightPPM := instance.Weight * 1_000_000 / instance.WeightScale.Denominator()

		if state.Phase != PhaseRegistered {
			reason := state.RemovalReason
			if reason == "" {
				reason = CodeExcludeNotRegistered
			}
			catalog.Excluded = append(catalog.Excluded, ExcludedInstance{
				InstanceID: instance.ID,
				Health:     state.Health,
				Reason:     reason,
			})
			continue
		}

		tier := 0
		effective := weightPPM
		basis := []string{CodeHealthyAdmitted}
		switch state.Health {
		case HealthHealthy:
		case HealthInitial:
			if !service.AdmitInitial {
				catalog.Excluded = append(catalog.Excluded, ExcludedInstance{
					InstanceID: instance.ID,
					Health:     state.Health,
					Reason:     CodeExcludeInitial,
				})
				continue
			}
			tier = 1
			basis = []string{CodeInitialAdmitted}
		case HealthUnhealthy:
			catalog.Excluded = append(catalog.Excluded, ExcludedInstance{
				InstanceID: instance.ID,
				Health:     state.Health,
				Reason:     CodeExcludeUnhealthy,
			})
			continue
		case HealthHalfOpen:
			switch service.HalfOpenPolicy {
			case HalfOpenExclude:
				catalog.Excluded = append(catalog.Excluded, ExcludedInstance{
					InstanceID: instance.ID,
					Health:     state.Health,
					Reason:     CodeExcludeHalfOpen,
				})
				continue
			case HalfOpenDemote:
				tier = 2
				effective = effective * service.HalfOpenFactorPermille / 1000
				basis = []string{CodeHalfOpenDemoted}
			}
		default:
			catalog.Excluded = append(catalog.Excluded, ExcludedInstance{
				InstanceID: instance.ID,
				Health:     state.Health,
				Reason:     CodeExcludeUnhealthy,
			})
			continue
		}
		catalog.Candidates = append(catalog.Candidates, Candidate{
			InstanceID:         instance.ID,
			Address:            instance.Address,
			Health:             state.Health,
			WeightPPM:          weightPPM,
			EffectiveWeightPPM: effective,
			DisplayWeight:      normalizedDisplayWeight(effective, displayScale),
			DisplayScale:       displayScale,
			Tier:               tier,
			Basis:              basis,
		})
	}
	sort.SliceStable(catalog.Candidates, func(i, j int) bool {
		left := catalog.Candidates[i]
		right := catalog.Candidates[j]
		if left.Tier != right.Tier {
			return left.Tier < right.Tier
		}
		if left.EffectiveWeightPPM != right.EffectiveWeightPPM {
			return left.EffectiveWeightPPM > right.EffectiveWeightPPM
		}
		leftInstance, _ := project.InstanceByID(left.InstanceID)
		rightInstance, _ := project.InstanceByID(right.InstanceID)
		return leftInstance.Order < rightInstance.Order
	})
	if limit > 0 && len(catalog.Candidates) > limit {
		catalog.Candidates = catalog.Candidates[:limit]
	}
	return catalog
}
