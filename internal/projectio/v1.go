package projectio

import (
	"encoding/json"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/session"
)

type dtoV1 struct {
	Schema            string      `json:"schema"`
	ProjectID         string      `json:"project_id"`
	Seed              int64       `json:"seed"`
	MasterFrame       rectV1      `json:"master_frame"`
	TotalBudgetKbps   int64       `json:"total_budget_kbps"`
	DispatchThreads   int         `json:"dispatch_threads"`
	BufferBudgetUnits int64       `json:"buffer_budget_units"`
	ScreensFlat       []screenV1  `json:"screens_flat"`
	Timeline          []wireEvent `json:"timeline"`
}

type rectV1 struct {
	X        int64 `json:"x"`
	Y        int64 `json:"y"`
	WidthPx  int64 `json:"width_px"`
	HeightPx int64 `json:"height_px"`
}

type screenV1 struct {
	ScreenID          string `json:"screen_id"`
	HostID            string `json:"host_id"`
	WidthPx           int64  `json:"width_px"`
	HeightPx          int64  `json:"height_px"`
	ScalePermille     int64  `json:"scale_permille"`
	MinScalePermille  int64  `json:"min_scale_permille"`
	ChromaPlanar      string `json:"chroma_planar"`
	DowngradePriority int64  `json:"downgrade_priority"`
}

func WriteV1(project session.Project) ([]byte, error) {
	dto := dtoV1{
		Schema: SchemaV1Wire, ProjectID: project.ProjectID, Seed: project.Seed,
		MasterFrame:     rectV1{X: project.Master.X, Y: project.Master.Y, WidthPx: project.Master.Width, HeightPx: project.Master.Height},
		TotalBudgetKbps: project.TotalBudget, DispatchThreads: project.Threads, BufferBudgetUnits: project.BufferBudget,
		ScreensFlat: make([]screenV1, 0, len(project.Screens)),
		Timeline:    make([]wireEvent, 0, len(project.Events)),
	}
	for _, screen := range project.Screens {
		scalePermille, err := permyriadToPermilleExact(screen.ScalePermyriad)
		if err != nil {
			return nil, err.(*bizerror.Error).WithScreen(screen.ID).WithPath("scale_permyriad")
		}
		minScalePermille, err := permyriadToPermilleExact(screen.MinScalePermyriad)
		if err != nil {
			return nil, err.(*bizerror.Error).WithScreen(screen.ID).WithPath("min_scale_permyriad")
		}
		dto.ScreensFlat = append(dto.ScreensFlat, screenV1{
			ScreenID: screen.ID, HostID: screen.Host, WidthPx: screen.Width, HeightPx: screen.Height,
			ScalePermille: scalePermille, MinScalePermille: minScalePermille,
			ChromaPlanar: string(mustChromaInfo(screen.Chroma).PlaneEnum), DowngradePriority: screen.Priority,
		})
	}
	for _, event := range project.Events {
		dto.Timeline = append(dto.Timeline, eventToWire(event))
	}
	return json.MarshalIndent(dto, "", "  ")
}

func loadV1(data []byte) (*session.Project, error) {
	var dto dtoV1
	if err := decodeJSON(data, &dto); err != nil {
		return nil, err
	}
	if dto.Schema != "" && dto.Schema != SchemaV1Wire {
		return nil, bizerror.New(bizerror.ESchemaUnsupported, "unsupported v1 schema identifier").WithDetail("schema", dto.Schema)
	}
	project := &session.Project{
		SchemaVersion: session.SchemaV1, ProjectID: dto.ProjectID, Seed: dto.Seed,
		Master:      session.Rect{X: dto.MasterFrame.X, Y: dto.MasterFrame.Y, Width: dto.MasterFrame.WidthPx, Height: dto.MasterFrame.HeightPx},
		TotalBudget: dto.TotalBudgetKbps, Threads: dto.DispatchThreads, BufferBudget: dto.BufferBudgetUnits,
		Screens: make([]session.Screen, 0, len(dto.ScreensFlat)),
	}
	for i, raw := range dto.ScreensFlat {
		scale, err := permilleFieldToPermyriad(raw.ScalePermille, "screens_flat["+itoa(i)+"].scale_permille")
		if err != nil {
			return nil, err
		}
		minScale, err := permilleFieldToPermyriad(raw.MinScalePermille, "screens_flat["+itoa(i)+"].min_scale_permille")
		if err != nil {
			return nil, err
		}
		if _, info, err := session.ParseChroma(raw.ChromaPlanar); err != nil {
			return nil, err.(*bizerror.Error).WithPath("screens_flat[" + itoa(i) + "].chroma_planar").WithScreen(raw.ScreenID)
		} else {
			_ = info
		}
		project.Screens = append(project.Screens, session.Screen{
			ID: raw.ScreenID, Host: raw.HostID, Width: raw.WidthPx, Height: raw.HeightPx,
			ScalePermyriad: scale, MinScalePermyriad: minScale, Chroma: session.ChromaFormat(normalizeChroma(raw.ChromaPlanar)),
			Priority: raw.DowngradePriority,
		})
	}
	events, err := eventsFromWire(dto.Timeline)
	if err != nil {
		return nil, err
	}
	project.Events = events
	if err := NormalizeLoaded(project); err != nil {
		return nil, err
	}
	return project, nil
}

func permilleFieldToPermyriad(permille int64, path string) (int64, error) {
	if permille == 0 {
		return 0, bizerror.New(bizerror.EScaleZero, "scale tier is zero").WithPath(path)
	}
	if permille < 0 || permille > session.ScaleBasePermille {
		return 0, bizerror.New(bizerror.EScaleOutOfRange, "permille scale is outside declared range").
			WithPath(path).WithDetail("value", permille).WithDetail("max", session.ScaleBasePermille)
	}
	return permilleToPermyriad(permille), nil
}

func normalizeChroma(raw string) string {
	format, _, err := session.ParseChroma(raw)
	if err != nil {
		return raw
	}
	return string(format)
}

func mustChromaInfo(format session.ChromaFormat) session.ChromaInfo {
	_, info, err := session.ParseChroma(string(format))
	if err != nil {
		return session.ChromaInfo{}
	}
	return info
}

func itoa(i int) string {
	return jsonNumber(i)
}
