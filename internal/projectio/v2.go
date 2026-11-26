package projectio

import (
	"encoding/json"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/session"
)

type dtoV2 struct {
	Schema            string      `json:"schema"`
	ProjectID         string      `json:"projectId"`
	Seed              int64       `json:"seed"`
	MasterFrame       rectV2      `json:"masterFrame"`
	TotalBudgetKbps   int64       `json:"totalBudgetKbps"`
	DispatchThreads   int         `json:"dispatchThreads"`
	BufferBudgetUnits int64       `json:"bufferBudgetUnits"`
	Hosts             []hostV2    `json:"hosts"`
	Timeline          []wireEvent `json:"timeline"`
}

type rectV2 struct {
	OriginX      int64 `json:"originX"`
	OriginY      int64 `json:"originY"`
	WidthPixels  int64 `json:"widthPixels"`
	HeightPixels int64 `json:"heightPixels"`
}

type hostV2 struct {
	HostID  string     `json:"hostId"`
	Screens []screenV2 `json:"screens"`
}

type screenV2 struct {
	ScreenID               string       `json:"screenId"`
	Resolution             resolutionV2 `json:"resolution"`
	ScalePerTenThousand    int64        `json:"scalePerTenThousand"`
	MinScalePerTenThousand int64        `json:"minScalePerTenThousand"`
	ChromaBitsPerPixel     int64        `json:"chromaBitsPerPixel"`
	DowngradePriority      int64        `json:"downgradePriority"`
}

type resolutionV2 struct {
	AreaKiloPixels int64 `json:"areaKiloPixels"`
	AspectWidth    int64 `json:"aspectWidth"`
	AspectHeight   int64 `json:"aspectHeight"`
}

func WriteV2(project session.Project) ([]byte, error) {
	dto := dtoV2{
		Schema: SchemaV2Wire, ProjectID: project.ProjectID, Seed: project.Seed,
		MasterFrame:     rectV2{OriginX: project.Master.X, OriginY: project.Master.Y, WidthPixels: project.Master.Width, HeightPixels: project.Master.Height},
		TotalBudgetKbps: project.TotalBudget, DispatchThreads: project.Threads, BufferBudgetUnits: project.BufferBudget,
		Hosts:    []hostV2{},
		Timeline: []wireEvent{},
	}
	hostIndex := map[string]int{}
	for _, screen := range project.Screens {
		area, err := AreaKiloPixels(screen.Width, screen.Height)
		if err != nil {
			return nil, err.(*bizerror.Error).WithScreen(screen.ID)
		}
		aw, ah := reducedAspect(screen.Width, screen.Height)
		idx, ok := hostIndex[screen.Host]
		if !ok {
			idx = len(dto.Hosts)
			hostIndex[screen.Host] = idx
			dto.Hosts = append(dto.Hosts, hostV2{HostID: screen.Host, Screens: []screenV2{}})
		}
		info := mustChromaInfo(screen.Chroma)
		dto.Hosts[idx].Screens = append(dto.Hosts[idx].Screens, screenV2{
			ScreenID:            screen.ID,
			Resolution:          resolutionV2{AreaKiloPixels: area, AspectWidth: aw, AspectHeight: ah},
			ScalePerTenThousand: screen.ScalePermyriad, MinScalePerTenThousand: screen.MinScalePermyriad,
			ChromaBitsPerPixel: info.BitsPerPixel, DowngradePriority: screen.Priority,
		})
	}
	for _, event := range project.Events {
		dto.Timeline = append(dto.Timeline, eventToWire(event))
	}
	return json.MarshalIndent(dto, "", "  ")
}

func loadV2(data []byte) (*session.Project, error) {
	var dto dtoV2
	if err := decodeJSON(data, &dto); err != nil {
		return nil, err
	}
	if dto.Schema != "" && dto.Schema != SchemaV2Wire {
		return nil, bizerror.New(bizerror.ESchemaUnsupported, "unsupported v2 schema identifier").WithDetail("schema", dto.Schema)
	}
	project := &session.Project{
		SchemaVersion: session.SchemaV2, ProjectID: dto.ProjectID, Seed: dto.Seed,
		Master:      session.Rect{X: dto.MasterFrame.OriginX, Y: dto.MasterFrame.OriginY, Width: dto.MasterFrame.WidthPixels, Height: dto.MasterFrame.HeightPixels},
		TotalBudget: dto.TotalBudgetKbps, Threads: dto.DispatchThreads, BufferBudget: dto.BufferBudgetUnits,
		Screens: []session.Screen{},
	}
	for hi, host := range dto.Hosts {
		for si, raw := range host.Screens {
			path := "hosts[" + itoa(hi) + "].screens[" + itoa(si) + "]"
			width, height, err := DimensionsFromArea(raw.Resolution.AreaKiloPixels, raw.Resolution.AspectWidth, raw.Resolution.AspectHeight)
			if err != nil {
				return nil, err.(*bizerror.Error).WithPath(path + ".resolution").WithScreen(raw.ScreenID)
			}
			if err := validatePerTenThousand(raw.ScalePerTenThousand); err != nil {
				return nil, err.WithPath(path + ".scalePerTenThousand").WithScreen(raw.ScreenID)
			}
			if err := validatePerTenThousand(raw.MinScalePerTenThousand); err != nil {
				return nil, err.WithPath(path + ".minScalePerTenThousand").WithScreen(raw.ScreenID)
			}
			format, _, err := session.ChromaFromBits(raw.ChromaBitsPerPixel)
			if err != nil {
				return nil, err.(*bizerror.Error).WithPath(path + ".chromaBitsPerPixel").WithScreen(raw.ScreenID)
			}
			project.Screens = append(project.Screens, session.Screen{
				ID: raw.ScreenID, Host: host.HostID, Width: width, Height: height,
				ScalePermyriad: raw.ScalePerTenThousand, MinScalePermyriad: raw.MinScalePerTenThousand,
				Chroma: format, Priority: raw.DowngradePriority,
			})
		}
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

func validatePerTenThousand(value int64) *bizerror.Error {
	if value == 0 {
		return bizerror.New(bizerror.EScaleZero, "scale tier is zero")
	}
	if value < 0 || value > session.ScaleBasePermyriad {
		return bizerror.New(bizerror.EScaleOutOfRange, "per-ten-thousand scale is outside declared range").
			WithDetail("value", value).WithDetail("max", session.ScaleBasePermyriad)
	}
	return nil
}

func reducedAspect(width, height int64) (int64, int64) {
	if width <= 0 || height <= 0 {
		return 1, 1
	}
	a, b := width, height
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return width, height
	}
	return width / a, height / a
}
