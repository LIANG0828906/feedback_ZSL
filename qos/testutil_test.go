package qos

func stepsEquivalent(a, b []StepResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if TruthStepDigest(a[i]) != TruthStepDigest(b[i]) {
			return false
		}
	}
	return true
}
