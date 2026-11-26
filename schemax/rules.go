package schemax

import (
	"encoding/json"

	"iconflow/errs"
	"iconflow/model"
)

func rulesFromRaw(raw json.RawMessage, path string, quantilePermyriad int) (model.Rules, *errs.Error) {
	rules := defaultRules()
	rules.QuantilePermyriad = quantilePermyriad
	if len(raw) == 0 {
		return rules, nil
	}
	obj, err := asObject(raw, path)
	if err != nil {
		return rules, err
	}
	if v := optionalString(obj, "operator"); v != "" {
		rules.GradientOperator = v
	}
	if v := optionalString(obj, "gradient_operator"); v != "" {
		rules.GradientOperator = v
	}
	if v := optionalString(obj, "operator_version"); v != "" {
		rules.OperatorVersion = v
	}
	if v := optionalString(obj, "algo_version"); v != "" {
		rules.OperatorVersion = v
	}
	rules.MinContrast = optionalInt(obj, "min_contrast", rules.MinContrast)
	rules.ThresholdMin = optionalInt(obj, "threshold_min", rules.ThresholdMin)
	rules.ThresholdMax = optionalInt(obj, "threshold_max", rules.ThresholdMax)
	rules.MinComponentArea = optionalInt(obj, "min_component_area", rules.MinComponentArea)
	rules.Connectivity = optionalInt(obj, "connectivity", rules.Connectivity)
	rules.FillHoles = optionalBool(obj, "fill_holes", rules.FillHoles)
	rules.Tolerances.AreaAbs = optionalInt(obj, "area_tolerance", rules.Tolerances.AreaAbs)
	rules.Tolerances.AreaAbs = optionalInt(obj, "area_abs", rules.Tolerances.AreaAbs)
	rules.Tolerances.CentroidMilliAbs = int64(optionalInt(obj, "centroid_tolerance_milli", int(rules.Tolerances.CentroidMilliAbs)))
	rules.Tolerances.CentroidMilliAbs = int64(optionalInt(obj, "centroid_milli_abs", int(rules.Tolerances.CentroidMilliAbs)))

	if tolRaw, ok := obj["tolerances"]; ok {
		tolObj, terr := asObject(tolRaw, path+".tolerances")
		if terr != nil {
			return rules, terr
		}
		rules.Tolerances.AreaAbs = optionalInt(tolObj, "area_abs", rules.Tolerances.AreaAbs)
		rules.Tolerances.CentroidMilliAbs = int64(optionalInt(tolObj, "centroid_milli_abs", int(rules.Tolerances.CentroidMilliAbs)))
	}
	if rules.Connectivity == 0 {
		rules.Connectivity = 8
	}
	return rules, nil
}
