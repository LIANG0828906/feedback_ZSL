package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"driftconverge/internal/model"
)

type scenarioCase struct {
	Name   string          `json:"name"`
	Report json.RawMessage `json:"report"`
}

type check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type output struct {
	Schema     string         `json:"schema"`
	Seed       int            `json:"seed"`
	DurationMS int64          `json:"duration_ms"`
	Cases      []scenarioCase `json:"cases"`
	Metrics    string         `json:"metrics"`
	Checks     []check        `json:"checks"`
	Passed     bool           `json:"passed"`
}

func main() {
	var (
		outPath     = flag.String("out", "artifacts/scenario-report.json", "output file")
		seed        = flag.Int("seed", 315, "sample seed")
		durationMS  = flag.Int64("duration-ms", 0, "scenario duration in milliseconds")
		metricsFile = flag.String("metrics", "", "Prometheus metrics snapshot")
	)
	flag.Parse()

	cases := make([]scenarioCase, 0)
	for _, argument := range flag.Args() {
		parts := strings.SplitN(argument, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			log.Fatalf("invalid case argument %q; expected name=path", argument)
		}
		data, err := os.ReadFile(parts[1])
		if err != nil {
			log.Fatalf("read %s: %v", parts[1], err)
		}
		if !json.Valid(data) {
			log.Fatalf("%s is not valid JSON", parts[1])
		}
		cases = append(cases, scenarioCase{Name: parts[0], Report: json.RawMessage(data)})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })

	metrics := ""
	if *metricsFile != "" {
		data, err := os.ReadFile(*metricsFile)
		if err != nil {
			log.Fatalf("read metrics: %v", err)
		}
		metrics = string(data)
	}

	checks := evaluate(cases, metrics)
	passed := true
	for _, item := range checks {
		if !item.Passed {
			passed = false
		}
	}
	result := output{
		Schema:     "driftconverge.scenario/v1",
		Seed:       *seed,
		DurationMS: *durationMS,
		Cases:      cases,
		Metrics:    metrics,
		Checks:     checks,
		Passed:     passed,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Fatalf("encode report: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(*outPath, data, 0o644); err != nil {
		log.Fatalf("write report: %v", err)
	}
	fmt.Printf("scenario report written to %s (passed=%t)\n", *outPath, passed)
	if !passed {
		for _, item := range checks {
			if !item.Passed {
				fmt.Printf("FAILED %s: %s\n", item.Name, item.Detail)
			}
		}
		os.Exit(1)
	}
}

func evaluate(cases []scenarioCase, metrics string) []check {
	checks := []check{
		checkClusterConclusion(cases, "01-baseline", "cluster-dev", model.ConclusionConverged, "baseline dev converged"),
		checkClusterConclusion(cases, "01-baseline", "cluster-prod", model.ConclusionConverged, "baseline prod converged"),
		checkPolicy(cases),
		checkEnvConflict(cases),
		checkStuck(cases),
		checkUnobservable(cases),
		checkRestart(cases),
		checkAPIStatus(cases),
		checkMetrics(metrics),
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	return checks
}

func roundCase(cases []scenarioCase, name string) (model.RoundReport, bool) {
	for _, item := range cases {
		if item.Name != name {
			continue
		}
		var report model.RoundReport
		if err := json.Unmarshal(item.Report, &report); err != nil {
			return model.RoundReport{}, false
		}
		if len(report.Clusters) == 0 {
			return model.RoundReport{}, false
		}
		return report, true
	}
	return model.RoundReport{}, false
}

func clusterIn(report model.RoundReport, cluster string) (model.ClusterReport, bool) {
	for _, item := range report.Clusters {
		if item.Cluster == cluster {
			return item, true
		}
	}
	return model.ClusterReport{}, false
}

func checkClusterConclusion(
	cases []scenarioCase,
	caseName, cluster, expected, display string,
) check {
	name := caseName + "." + cluster
	report, ok := roundCase(cases, caseName)
	if !ok {
		return check{Name: name, Passed: false, Detail: "case is missing or is not a round report"}
	}
	item, found := clusterIn(report, cluster)
	if !found {
		return check{Name: name, Passed: false, Detail: "cluster is missing"}
	}
	passed := item.Convergence == expected
	return check{
		Name:   name,
		Passed: passed,
		Detail: display + ": expected=" + expected + " actual=" + item.Convergence,
	}
}

func checkPolicy(cases []scenarioCase) check {
	name := "02-policy.prod-rejected"
	report, ok := roundCase(cases, "02-policy")
	if !ok {
		return check{Name: name, Passed: false, Detail: "case is missing"}
	}
	item, found := clusterIn(report, "cluster-prod")
	if !found {
		return check{Name: name, Passed: false, Detail: "prod cluster is missing"}
	}
	passed := item.ErrorCode == model.CodePolicy &&
		item.PolicyBlockCount >= 3 &&
		item.Convergence == model.ConclusionFailed &&
		len(item.ApplyResults) == 0
	return check{
		Name:   name,
		Passed: passed,
		Detail: fmt.Sprintf(
			"expected E_POLICY, >=3 blocks, failed, no apply; got code=%s blocks=%d conclusion=%s applies=%d",
			item.ErrorCode,
			item.PolicyBlockCount,
			item.Convergence,
			len(item.ApplyResults),
		),
	}
}

func checkEnvConflict(cases []scenarioCase) check {
	name := "03-env-conflict.prod-repaired"
	report, ok := roundCase(cases, "03-env-conflict")
	if !ok {
		return check{Name: name, Passed: false, Detail: "case is missing"}
	}
	item, found := clusterIn(report, "cluster-prod")
	if !found {
		return check{Name: name, Passed: false, Detail: "prod cluster is missing"}
	}
	hasEnvDiff := false
	for _, difference := range item.Differences {
		if difference.Field == "env" && difference.EnvKey == "APP_MODE" && difference.Resolution == model.ResolutionConverged {
			hasEnvDiff = true
		}
	}
	passed := item.DiffCount >= 1 &&
		item.Convergence == model.ConclusionConverged &&
		hasEnvDiff &&
		len(item.ApplyResults) >= 1
	return check{
		Name:   name,
		Passed: passed,
		Detail: fmt.Sprintf(
			"expected observed env diff and converged; got diffs=%d env_diff=%t conclusion=%s applies=%d",
			item.DiffCount,
			hasEnvDiff,
			item.Convergence,
			len(item.ApplyResults),
		),
	}
}

func checkStuck(cases []scenarioCase) check {
	name := "04-stuck-rollout.prod-wait"
	report, ok := roundCase(cases, "04-stuck-rollout")
	if !ok {
		return check{Name: name, Passed: false, Detail: "case is missing"}
	}
	item, found := clusterIn(report, "cluster-prod")
	if !found {
		return check{Name: name, Passed: false, Detail: "prod cluster is missing"}
	}
	hasBlocked := false
	for _, difference := range item.Differences {
		if difference.Resolution == model.ResolutionBlocked {
			hasBlocked = true
		}
	}
	passed := item.ErrorCode == model.CodeWait &&
		item.BlockedCount > 0 &&
		hasBlocked &&
		(item.Convergence == model.ConclusionPartial || item.Convergence == model.ConclusionFailed)
	return check{
		Name:   name,
		Passed: passed,
		Detail: fmt.Sprintf(
			"expected E_WAIT and blocked differences; got code=%s blocked=%d conclusion=%s",
			item.ErrorCode,
			item.BlockedCount,
			item.Convergence,
		),
	}
}

func checkUnobservable(cases []scenarioCase) check {
	name := "06-unobservable.prod-preserved"
	prior, priorOK := roundCase(cases, "05-unobservable-baseline")
	current, currentOK := roundCase(cases, "06-unobservable")
	if !priorOK || !currentOK {
		return check{Name: name, Passed: false, Detail: "required cases are missing"}
	}
	priorProd, priorFound := clusterIn(prior, "cluster-prod")
	currentProd, currentFound := clusterIn(current, "cluster-prod")
	if !priorFound || !currentFound {
		return check{Name: name, Passed: false, Detail: "prod cluster is missing"}
	}
	passed := !currentProd.Observable &&
		currentProd.ErrorCode == model.CodeCluster &&
		currentProd.DiffCount == priorProd.DiffCount &&
		currentProd.LastObservedRoundID == prior.RoundID
	return check{
		Name:   name,
		Passed: passed,
		Detail: fmt.Sprintf(
			"expected unobservable with preserved diff_count=%d and last_observed=%s; got observable=%t code=%s diffs=%d last_observed=%s",
			priorProd.DiffCount,
			prior.RoundID,
			currentProd.Observable,
			currentProd.ErrorCode,
			currentProd.DiffCount,
			currentProd.LastObservedRoundID,
		),
	}
}

func checkRestart(cases []scenarioCase) check {
	name := "07-restart.prod-converged"
	report, ok := roundCase(cases, "07-restart")
	if !ok {
		return check{Name: name, Passed: false, Detail: "case is missing"}
	}
	prod, found := clusterIn(report, "cluster-prod")
	if !found {
		return check{Name: name, Passed: false, Detail: "prod cluster is missing"}
	}
	previous, previousOK := roundCase(cases, "06-unobservable")
	passed := prod.Convergence == model.ConclusionConverged &&
		report.RoundID != "" &&
		(!previousOK || report.RoundID != previous.RoundID)
	return check{
		Name:   name,
		Passed: passed,
		Detail: fmt.Sprintf(
			"expected a new converged round after restart; got round=%s conclusion=%s",
			report.RoundID,
			prod.Convergence,
		),
	}
}

func checkAPIStatus(cases []scenarioCase) check {
	name := "08-api.prod-status"
	for _, item := range cases {
		if item.Name != "08-api-prod-status" {
			continue
		}
		var status model.ClusterStatus
		if err := json.Unmarshal(item.Report, &status); err != nil {
			return check{Name: name, Passed: false, Detail: err.Error()}
		}
		passed := status.Cluster == "cluster-prod" &&
			status.RoundID != "" &&
			status.Convergence == model.ConclusionConverged &&
			status.ErrorCode == ""
		return check{
			Name:   name,
			Passed: passed,
			Detail: fmt.Sprintf(
				"expected persisted prod status; got cluster=%s round=%s conclusion=%s code=%s",
				status.Cluster,
				status.RoundID,
				status.Convergence,
				status.ErrorCode,
			),
		}
	}
	return check{Name: name, Passed: false, Detail: "status case is missing"}
}

func checkMetrics(metrics string) check {
	name := "09-metrics.required-series"
	required := []string{
		`drift_expected_resources{cluster="cluster-dev"}`,
		`drift_differences{cluster="cluster-prod"}`,
		`drift_policy_blocks{cluster="cluster-prod"}`,
		`drift_blocked_items{cluster="cluster-prod"}`,
		`drift_converged_resources{cluster="cluster-prod"}`,
		`drift_unobservable{cluster="cluster-prod"}`,
	}
	missing := make([]string, 0)
	for _, series := range required {
		if !strings.Contains(metrics, series) {
			missing = append(missing, series)
		}
	}
	return check{
		Name:   name,
		Passed: len(missing) == 0,
		Detail: "missing metrics: " + strings.Join(missing, ", "),
	}
}
