package verify

import (
	"fmt"
	"reflect"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/generator"
	"mirrorcast/internal/projectio"
	"mirrorcast/internal/safemath"
	"mirrorcast/internal/session"
)

type PrecisionItem struct {
	ProjectID            string             `json:"project_id"`
	Tick                 int64              `json:"tick"`
	ScreenID             string             `json:"screen_id"`
	Truth                *session.Mapping   `json:"truth,omitempty"`
	Calculated           session.Mapping    `json:"calculated"`
	RectangleMaxDiff     int64              `json:"rectangle_max_diff_pixels"`
	PlaneMaxDiff         int64              `json:"plane_max_diff_pixels"`
	BudgetDiffKbps       int64              `json:"budget_diff_kbps"`
	Tolerance            session.Tolerances `json:"tolerance"`
	WithinTolerance      bool               `json:"within_tolerance"`
	ScalePermyriad       int64              `json:"scale_permyriad"`
	ScalePermille        int64              `json:"scale_permille"`
	ScaleConversionExact bool               `json:"scale_conversion_exact"`
	AreaKiloPixels       int64              `json:"area_kilo_pixels,omitempty"`
	AreaConversionOK     bool               `json:"area_conversion_ok"`
	ChromaBitsPerPixel   int64              `json:"chroma_bits_per_pixel"`
	OverflowChecked      bool               `json:"overflow_checked"`
	Int32Saturated       bool               `json:"int32_saturated"`
	SaturatedFields      []string           `json:"saturated_fields,omitempty"`
}

type CriticalReport struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	OK           bool   `json:"ok"`
	ExpectedCode string `json:"expected_code,omitempty"`
	ActualCode   string `json:"actual_code,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

type ProjectReport struct {
	Name               string          `json:"name"`
	ProjectID          string          `json:"project_id"`
	VersionsEquivalent bool            `json:"versions_equivalent"`
	TruthMatches       bool            `json:"truth_matches"`
	ScreenCount        int             `json:"screen_count"`
	TickCount          int             `json:"tick_count"`
	Mismatches         []string        `json:"mismatches,omitempty"`
	Precision          []PrecisionItem `json:"precision,omitempty"`
}

type Report struct {
	OK       bool             `json:"ok"`
	Seed     int64            `json:"seed"`
	Projects []ProjectReport  `json:"projects"`
	Critical []CriticalReport `json:"critical"`
}

func VerifySuite(suite *generator.Suite) (*Report, error) {
	report := &Report{OK: true, Seed: suite.Seed}
	for _, generated := range suite.Projects {
		pr := ProjectReport{Name: generated.Name, ProjectID: generated.Project.ProjectID, VersionsEquivalent: true, TruthMatches: true}
		versions := map[string]*session.Project{}
		for _, version := range []string{session.SchemaV1, session.SchemaV2, session.SchemaV3} {
			data, err := projectio.Write(generated.Project, version)
			if err != nil {
				return nil, fmt.Errorf("write %s/%s: %w", generated.Name, version, err)
			}
			loaded, err := projectio.LoadVersion(data, version)
			if err != nil {
				return nil, fmt.Errorf("load %s/%s: %w", generated.Name, version, err)
			}
			versions[version] = loaded
		}
		base := versions[session.SchemaV1]
		pr.ScreenCount = len(base.Screens)
		for _, version := range []string{session.SchemaV2, session.SchemaV3} {
			if mismatch := projectMismatch(base, versions[version]); mismatch != "" {
				pr.VersionsEquivalent = false
				pr.Mismatches = append(pr.Mismatches, version+": "+mismatch)
			}
		}
		for _, version := range []string{session.SchemaV1, session.SchemaV2, session.SchemaV3} {
			truth, err := generator.BuildTruth(*versions[version])
			if err != nil {
				return nil, fmt.Errorf("truth %s/%s: %w", generated.Name, version, err)
			}
			if !reflect.DeepEqual(truth.Ticks, generated.Truth.Ticks) {
				pr.TruthMatches = false
				pr.Mismatches = append(pr.Mismatches, "truth mismatch in schema "+version)
			}
		}
		pr.TickCount = len(generated.Truth.Ticks)
		pr.Precision = BuildPrecision(generated.Project, generated.Truth)
		for _, item := range pr.Precision {
			if !item.WithinTolerance {
				pr.Mismatches = append(pr.Mismatches, fmt.Sprintf("precision tolerance exceeded at tick %d screen %s", item.Tick, item.ScreenID))
			}
		}
		if !pr.VersionsEquivalent || !pr.TruthMatches {
			report.OK = false
		}
		report.Projects = append(report.Projects, pr)
	}
	for _, critical := range suite.Critical.Cases {
		cr := RunCritical(critical)
		if !cr.OK {
			report.OK = false
		}
		report.Critical = append(report.Critical, cr)
	}
	return report, nil
}

func projectMismatch(a, b *session.Project) string {
	ac, bc := *a, *b
	ac.SchemaVersion, bc.SchemaVersion = "", ""
	if !reflect.DeepEqual(ac.Master, bc.Master) {
		return "master region differs"
	}
	if ac.TotalBudget != bc.TotalBudget || ac.Threads != bc.Threads || ac.BufferBudget != bc.BufferBudget {
		return "budget/runtime fields differ"
	}
	if !reflect.DeepEqual(ac.Screens, bc.Screens) {
		return fmt.Sprintf("screens differ: %d vs %d", len(ac.Screens), len(bc.Screens))
	}
	if !reflect.DeepEqual(ac.Events, bc.Events) {
		return "events/topology differ"
	}
	if ac.ProjectID != bc.ProjectID || ac.Seed != bc.Seed {
		return "identity/seed differs"
	}
	return ""
}

func BuildPrecision(project session.Project, truth session.TruthFile) []PrecisionItem {
	items := []PrecisionItem{}
	screenByID := map[string]session.Screen{}
	for _, screen := range project.Screens {
		screenByID[screen.ID] = screen
	}
	for _, tick := range truth.Ticks {
		for _, result := range tick.Results {
			if result.Mapping == nil || result.SettledScale == 0 {
				continue
			}
			screen := screenByID[result.ScreenID]
			calculated, mapErr := session.MapRegion(project.Master, screen.Width, screen.Height, result.SettledScale, string(result.Mapping.Chroma))
			item := PrecisionItem{
				ProjectID: project.ProjectID, Tick: tick.Tick, ScreenID: result.ScreenID,
				Truth: result.Mapping, Calculated: calculated, Tolerance: truth.Tolerances,
				ScalePermyriad: result.SettledScale, ScalePermille: result.SettledScale / 10,
				ScaleConversionExact: result.SettledScale%10 == 0,
				ChromaBitsPerPixel:   calculated.NominalBitsPerPixel, OverflowChecked: true,
			}
			item.RectangleMaxDiff = maxRectDiff(result.Mapping.Destination, calculated.Destination)
			item.PlaneMaxDiff = maxInt64(safemath.AbsDiff(result.Mapping.PlaneWidth, calculated.PlaneWidth), safemath.AbsDiff(result.Mapping.PlaneHeight, calculated.PlaneHeight))
			item.BudgetDiffKbps = safemath.AbsDiff(result.Mapping.BudgetCostKbps, calculated.BudgetCostKbps)
			item.WithinTolerance = item.RectangleMaxDiff <= truth.Tolerances.RectanglePixels &&
				item.PlaneMaxDiff <= truth.Tolerances.PlanePixels &&
				item.BudgetDiffKbps <= truth.Tolerances.BudgetKbps
			if area, areaErr := projectio.AreaKiloPixels(screen.Width, screen.Height); areaErr == nil {
				item.AreaKiloPixels = area
				item.AreaConversionOK = true
			} else {
				item.WithinTolerance = false
			}
			if mapErr != nil {
				item.WithinTolerance = false
				item.PlaneMaxDiff = -1
			}
			saturationChecks := []struct {
				field string
				value int64
			}{
				{"destination.x", calculated.Destination.X}, {"destination.y", calculated.Destination.Y},
				{"destination.width", calculated.Destination.Width}, {"destination.height", calculated.Destination.Height},
				{"plane.width", calculated.PlaneWidth}, {"plane.height", calculated.PlaneHeight},
			}
			for _, check := range saturationChecks {
				if _, saturated := safemath.SaturateInt32(check.value); saturated {
					item.Int32Saturated = true
					item.SaturatedFields = append(item.SaturatedFields, check.field)
				}
			}
			items = append(items, item)
		}
	}
	return items
}

func maxRectDiff(a, b session.Rect) int64 {
	return maxInt64(
		safemath.AbsDiff(a.X, b.X), safemath.AbsDiff(a.Y, b.Y),
		safemath.AbsDiff(a.Width, b.Width), safemath.AbsDiff(a.Height, b.Height),
	)
}

func maxInt64(values ...int64) int64 {
	best := values[0]
	for _, value := range values[1:] {
		if value > best {
			best = value
		}
	}
	return best
}

func RunCritical(test generator.CriticalCase) CriticalReport {
	report := CriticalReport{ID: test.ID, Kind: test.Kind, ExpectedCode: test.ExpectedCode}
	input := test.Input
	master := session.Rect{X: 0, Y: 0, Width: 16, Height: 9}
	if input.Master != nil {
		master = *input.Master
	}

	switch test.Kind {
	case "mapping_error":
		_, err := session.MapRegion(master, input.Width, input.Height, input.Scale, input.Chroma)
		report.ActualCode = bizerror.CodeOf(err)
		report.OK = report.ActualCode == test.ExpectedCode
		if err != nil {
			report.Detail = err.Error()
		}
	case "mapping":
		mapping, err := session.MapRegion(master, input.Width, input.Height, input.Scale, input.Chroma)
		if err != nil {
			report.ActualCode = bizerror.CodeOf(err)
			report.OK = false
			report.Detail = err.Error()
			return report
		}
		report.OK = test.Expected != nil && reflect.DeepEqual(mapping, *test.Expected)
		if !report.OK {
			report.Detail = fmt.Sprintf("mapping mismatch: got %+v want %+v", mapping, test.Expected)
		}
	case "budget_boundary":
		mapping, err := session.MapRegion(master, input.Width, input.Height, input.Scale, input.Chroma)
		if err != nil {
			report.ActualCode = bizerror.CodeOf(err)
			report.OK = false
			report.Detail = err.Error()
			return report
		}
		decision := session.DecisionRejected
		if mapping.BudgetCostKbps <= input.Budget {
			decision = session.DecisionAdmitted
		}
		report.OK = decision == test.ExpectedDecision && reflect.DeepEqual(mapping, *test.Expected)
		report.Detail = fmt.Sprintf("cost=%d budget=%d decision=%s", mapping.BudgetCostKbps, input.Budget, decision)
	case "project_error":
		project := validCriticalProject()
		if input.Threads != 0 {
			project.Threads = input.Threads
		}
		if input.BufferBudget != 0 {
			project.BufferBudget = input.BufferBudget
		}
		err := session.ValidateProject(&project)
		report.ActualCode = bizerror.CodeOf(err)
		report.OK = report.ActualCode == test.ExpectedCode
		if err != nil {
			report.Detail = err.Error()
		}
	default:
		report.OK = false
		report.Detail = "unknown critical case kind"
	}
	return report
}

func validCriticalProject() session.Project {
	return session.Project{
		ProjectID:    "critical-validation",
		Master:       session.Rect{X: 0, Y: 0, Width: 320, Height: 180},
		TotalBudget:  1_000_000,
		Threads:      4,
		BufferBudget: 1 << 20,
		Screens: []session.Screen{{
			ID: "s1", Host: "h1", Width: 320, Height: 200,
			ScalePermyriad: 10000, MinScalePermyriad: 2500, Chroma: session.Chroma420, Priority: 1,
		}},
	}
}
