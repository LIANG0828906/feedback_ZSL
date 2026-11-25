package sample

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"

	"grayrelease/internal/engine"
	"grayrelease/internal/loader"
	"grayrelease/internal/model"
)

const (
	Seed               int64 = 4208691
	EngineeringVersion       = "eng-deterministic-v1"
	TickCount                = 6
	KeyCount                 = 16
)

func CanonicalProject() *model.Project {
	return &model.Project{
		SchemaVersion:      model.SchemaV2,
		EngineeringVersion: EngineeringVersion,
		Name:               "checkout-gray-kernel-sample",
		TickIntervalMS:     100,
		Services: []model.Service{
			{Name: "checkout", Baseline: "checkout-main:8080", Canary: "checkout-gray:8080", Dependencies: []string{"payment", "inventory"}},
			{Name: "payment", Baseline: "payment-main:8080", Canary: "payment-gray:8080", Dependencies: []string{"ledger"}},
			{Name: "inventory", Baseline: "inventory-main:8080", Canary: "inventory-gray:8080"},
			{Name: "ledger", Baseline: "ledger-main:8080", Canary: "ledger-gray:8080"},
		},
		Rules: []model.Rule{
			{ID: "R-CHK-BETA", RuleNo: 101, Service: "checkout", Priority: 100, Order: 0, Match: model.Match{Labels: map[string]string{"lane": "beta"}}, WeightBPS: 5000},
			{ID: "R-CHK-DEFAULT", RuleNo: 102, Service: "checkout", Priority: 10, Order: 1, Match: model.Match{}, WeightBPS: 2000},
			{ID: "R-PAY-VIP", RuleNo: 201, Service: "payment", Priority: 100, Order: 0, Match: model.Match{Labels: map[string]string{"tier": "vip"}}, WeightBPS: 10000},
			{ID: "R-PAY-DEFAULT", RuleNo: 202, Service: "payment", Priority: 10, Order: 1, Match: model.Match{}, WeightBPS: 0},
			{ID: "R-INV-CN", RuleNo: 301, Service: "inventory", Priority: 50, Order: 0, Match: model.Match{Labels: map[string]string{"region": "cn"}}, WeightBPS: 2500},
			{ID: "R-INV-DEFAULT", RuleNo: 302, Service: "inventory", Priority: 10, Order: 1, Match: model.Match{}, WeightBPS: 1000},
			{ID: "R-LED-AUDIT", RuleNo: 401, Service: "ledger", Priority: 60, Order: 0, Match: model.Match{Labels: map[string]string{"audit": "true"}}, WeightBPS: 0},
			{ID: "R-LED-DEFAULT", RuleNo: 402, Service: "ledger", Priority: 10, Order: 1, Match: model.Match{}, WeightBPS: 7500},
		},
		Mirrors: []model.MirrorPolicy{
			{Service: "checkout", RuleID: "R-CHK-BETA", RatioBPS: 3000},
			{Service: "payment", RuleID: "R-PAY-VIP", RatioBPS: 10000},
			{Service: "inventory", RuleID: "R-INV-CN", RatioBPS: 5000},
			{Service: "ledger", RuleID: "R-LED-DEFAULT", RatioBPS: 1500},
		},
	}
}

func KeyUniverse() []string {
	keys := make([]string, 0, KeyCount)
	for i := 1; i <= KeyCount; i++ {
		keys = append(keys, keyName(i))
	}
	return keys
}

func keyName(i int) string {
	return "key-" + pad3(i)
}

func pad3(i int) string {
	if i >= 100 {
		return itoa(i)
	}
	if i >= 10 {
		return "0" + itoa(i)
	}
	return "00" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func GeneratedRequests() []model.Request {
	rng := rand.New(rand.NewSource(Seed))
	keys := KeyUniverse()
	services := []string{"checkout", "payment", "inventory", "ledger"}
	profiles := make([]map[string]string, len(keys))
	for i := range keys {
		labels := map[string]string{}
		if rng.Intn(100) < 38 {
			labels["lane"] = "beta"
		}
		if rng.Intn(100) < 31 {
			labels["tier"] = "vip"
		}
		if rng.Intn(100) < 56 {
			labels["region"] = "cn"
		}
		if rng.Intn(100) < 25 {
			labels["audit"] = "true"
		}
		profiles[i] = labels
	}

	requests := make([]model.Request, 0, TickCount*len(services)*len(keys))
	sequence := 0
	for tick := 0; tick < TickCount; tick++ {
		for _, service := range services {
			for i, key := range keys {
				labels := cloneLabels(profiles[i])
				req := model.Request{
					Sequence:  sequence,
					Tick:      tick,
					RequestID: "t" + pad2(tick) + "-" + service + "-" + key,
					Service:   service,
					Key:       key,
					Labels:    labels,
				}
				requests = append(requests, req)
				sequence++
			}
		}
	}
	return requests
}

func pad2(i int) string {
	if i >= 10 {
		return itoa(i)
	}
	return "0" + itoa(i)
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type RuleTruth struct {
	ID         string `json:"id"`
	Service    string `json:"service"`
	RuleNo     int    `json:"rule_no"`
	Priority   int    `json:"priority"`
	Order      int    `json:"order"`
	WeightBPS  int    `json:"weight_bps"`
	V1Permille int    `json:"v1_permille"`
	V2BPS      int    `json:"v2_bps"`
	V3BPS      int    `json:"v3_bps"`
}

type ParseTruth struct {
	Seed               int64              `json:"seed"`
	EngineeringVersion string             `json:"engineering_version"`
	SchemaVersions     []string           `json:"schema_versions"`
	ServiceNames       []string           `json:"service_names"`
	RuleOrder          []RuleTruth        `json:"rule_order"`
	Keys               []string           `json:"keys"`
	RequestCount       int                `json:"request_count"`
	TickCount          int                `json:"tick_count"`
	CanonicalProject   *model.Project     `json:"canonical_project"`
	MigrationTruth     []engine.Migration `json:"migration_truth"`
}

func BuildTruth() (*ParseTruth, error) {
	project := CanonicalProject()
	keys := KeyUniverse()
	oldProject := CanonicalProject()
	newProject := CanonicalProject()
	for i := range newProject.Rules {
		if newProject.Rules[i].ID == "R-CHK-BETA" {
			newProject.Rules[i].WeightBPS = 6500
		}
	}
	migrations, err := engine.EnumerateMigrations(oldProject, newProject, "checkout", "R-CHK-BETA", keys)
	if err != nil {
		return nil, err
	}

	truth := &ParseTruth{
		Seed: Seed, EngineeringVersion: EngineeringVersion,
		SchemaVersions: []string{model.SchemaV1, model.SchemaV2, model.SchemaV3},
		Keys:           keys, RequestCount: TickCount * len(project.Services) * KeyCount, TickCount: TickCount,
		CanonicalProject: project, MigrationTruth: migrations,
	}
	for _, service := range project.Services {
		truth.ServiceNames = append(truth.ServiceNames, service.Name)
	}
	for _, rule := range project.Rules {
		truth.RuleOrder = append(truth.RuleOrder, RuleTruth{
			ID: rule.ID, Service: rule.Service, RuleNo: rule.RuleNo, Priority: rule.Priority,
			Order: rule.Order, WeightBPS: rule.WeightBPS, V1Permille: rule.WeightBPS / 10,
			V2BPS: rule.WeightBPS, V3BPS: rule.WeightBPS,
		})
	}
	return truth, nil
}

func WriteSet(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	project := CanonicalProject()
	requests := GeneratedRequests()
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		projectData, err := loader.MarshalProject(project, version)
		if err != nil {
			return err
		}
		requestData, err := loader.MarshalRequests(requests, version, project.TickIntervalMS)
		if err != nil {
			return err
		}
		suffix := map[string]string{model.SchemaV1: "v1", model.SchemaV2: "v2", model.SchemaV3: "v3"}[version]
		if err := os.WriteFile(filepath.Join(dir, "project-"+suffix+".json"), projectData, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "requests-"+suffix+".json"), requestData, 0o644); err != nil {
			return err
		}
	}
	truth, err := BuildTruth()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "truth.json"), data, 0o644)
}
