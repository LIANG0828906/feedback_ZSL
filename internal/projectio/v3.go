package projectio

import (
	"encoding/json"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/session"
)

type dtoV3 struct {
	SchemaVersion     string      `json:"schemaVersion"`
	SessionID         string      `json:"sessionId"`
	DeterministicSeed int64       `json:"deterministicSeed"`
	CaptureRegion     rectV3      `json:"captureRegion"`
	DeliveryBudget    budgetV3    `json:"deliveryBudget"`
	TopologyGroups    []groupV3   `json:"topologyGroups"`
	LogicalTimeline   []wireEvent `json:"logicalTimeline"`
}

type rectV3 struct {
	OriginX      int64 `json:"originX"`
	OriginY      int64 `json:"originY"`
	WidthPixels  int64 `json:"widthPixels"`
	HeightPixels int64 `json:"heightPixels"`
}

type budgetV3 struct {
	TotalKbps     int64 `json:"totalKbps"`
	WorkerThreads int   `json:"workerThreads"`
	BufferUnits   int64 `json:"bufferUnits"`
}

type groupV3 struct {
	GroupID  string      `json:"groupId"`
	Displays []displayV3 `json:"displays"`
}

type displayV3 struct {
	DisplayID                string   `json:"displayId"`
	WidthPixels              int64    `json:"widthPixels"`
	HeightPixels             int64    `json:"heightPixels"`
	EquivalentAreaKiloPixels *int64   `json:"equivalentAreaKiloPixels,omitempty"`
	ScalePermyriad           int64    `json:"scalePermyriad"`
	MinimumScalePermyriad    int64    `json:"minimumScalePermyriad"`
	Chroma                   chromaV3 `json:"chroma"`
	DowngradePriority        int64    `json:"downgradePriority"`
}

type chromaV3 struct {
	PlanarFormat        string `json:"planarFormat"`
	DerivedBitsPerPixel *int64 `json:"derivedBitsPerPixel,omitempty"`
}

func WriteV3(project session.Project) ([]byte, error) {
	dto := dtoV3{
		SchemaVersion: SchemaV3Wire, SessionID: project.ProjectID, DeterministicSeed: project.Seed,
		CaptureRegion:   rectV3{OriginX: project.Master.X, OriginY: project.Master.Y, WidthPixels: project.Master.Width, HeightPixels: project.Master.Height},
		DeliveryBudget:  budgetV3{TotalKbps: project.TotalBudget, WorkerThreads: project.Threads, BufferUnits: project.BufferBudget},
		TopologyGroups:  []groupV3{},
		LogicalTimeline: []wireEvent{},
	}
	groupIndex := map[string]int{}
	for _, screen := range project.Screens {
		area, err := AreaKiloPixels(screen.Width, screen.Height)
		if err != nil {
			return nil, err.(*bizerror.Error).WithScreen(screen.ID)
		}
		idx, ok := groupIndex[screen.Host]
		if !ok {
			idx = len(dto.TopologyGroups)
			groupIndex[screen.Host] = idx
			dto.TopologyGroups = append(dto.TopologyGroups, groupV3{GroupID: screen.Host, Displays: []displayV3{}})
		}
		info := mustChromaInfo(screen.Chroma)
		bits := info.BitsPerPixel
		dto.TopologyGroups[idx].Displays = append(dto.TopologyGroups[idx].Displays, displayV3{
			DisplayID: screen.ID, WidthPixels: screen.Width, HeightPixels: screen.Height,
			EquivalentAreaKiloPixels: &area, ScalePermyriad: screen.ScalePermyriad, MinimumScalePermyriad: screen.MinScalePermyriad,
			Chroma: chromaV3{PlanarFormat: info.PlaneEnum, DerivedBitsPerPixel: &bits}, DowngradePriority: screen.Priority,
		})
	}
	for _, event := range project.Events {
		dto.LogicalTimeline = append(dto.LogicalTimeline, eventToWire(event))
	}
	return json.MarshalIndent(dto, "", "  ")
}

func loadV3(data []byte) (*session.Project, error) {
	var dto dtoV3
	if err := decodeJSON(data, &dto); err != nil {
		return nil, err
	}
	if dto.SchemaVersion != "" && dto.SchemaVersion != SchemaV3Wire {
		return nil, bizerror.New(bizerror.ESchemaUnsupported, "unsupported v3 schema identifier").WithDetail("schema", dto.SchemaVersion)
	}
	project := &session.Project{
		SchemaVersion: session.SchemaV3, ProjectID: dto.SessionID, Seed: dto.DeterministicSeed,
		Master:      session.Rect{X: dto.CaptureRegion.OriginX, Y: dto.CaptureRegion.OriginY, Width: dto.CaptureRegion.WidthPixels, Height: dto.CaptureRegion.HeightPixels},
		TotalBudget: dto.DeliveryBudget.TotalKbps, Threads: dto.DeliveryBudget.WorkerThreads, BufferBudget: dto.DeliveryBudget.BufferUnits,
		Screens: []session.Screen{},
	}
	for gi, group := range dto.TopologyGroups {
		for di, raw := range group.Displays {
			path := "topologyGroups[" + itoa(gi) + "].displays[" + itoa(di) + "]"
			if raw.EquivalentAreaKiloPixels != nil {
				area, err := AreaKiloPixels(raw.WidthPixels, raw.HeightPixels)
				if err != nil {
					return nil, err.(*bizerror.Error).WithPath(path + ".equivalentAreaKiloPixels").WithScreen(raw.DisplayID)
				}
				if area != *raw.EquivalentAreaKiloPixels {
					return nil, bizerror.New(bizerror.EResolutionConflict, "pixel dimensions and equivalent kilopixel area disagree").
						WithPath(path+".equivalentAreaKiloPixels").
						WithScreen(raw.DisplayID).
						WithDetail("area_from_pixels", area).
						WithDetail("area_field", *raw.EquivalentAreaKiloPixels)
				}
			}
			if err := validatePerTenThousand(raw.ScalePermyriad); err != nil {
				return nil, err.WithPath(path + ".scalePermyriad").WithScreen(raw.DisplayID)
			}
			if err := validatePerTenThousand(raw.MinimumScalePermyriad); err != nil {
				return nil, err.WithPath(path + ".minimumScalePermyriad").WithScreen(raw.DisplayID)
			}
			format, err := resolveV3Chroma(raw.Chroma, path+".chroma", raw.DisplayID)
			if err != nil {
				return nil, err
			}
			groupID := group.GroupID
			project.Screens = append(project.Screens, session.Screen{
				ID: raw.DisplayID, Host: groupID, Width: raw.WidthPixels, Height: raw.HeightPixels,
				ScalePermyriad: raw.ScalePermyriad, MinScalePermyriad: raw.MinimumScalePermyriad,
				Chroma: format, Priority: raw.DowngradePriority,
			})
		}
	}
	events, err := eventsFromWire(dto.LogicalTimeline)
	if err != nil {
		return nil, err
	}
	project.Events = events
	if err := NormalizeLoaded(project); err != nil {
		return nil, err
	}
	return project, nil
}

func resolveV3Chroma(raw chromaV3, path, screenID string) (session.ChromaFormat, error) {
	var enumFormat session.ChromaFormat
	var enumSet bool
	if raw.PlanarFormat != "" {
		format, _, err := session.ParseChroma(raw.PlanarFormat)
		if err != nil {
			return "", err.(*bizerror.Error).WithPath(path + ".planarFormat").WithScreen(screenID)
		}
		enumFormat = format
		enumSet = true
	}
	var bitsFormat session.ChromaFormat
	var bitsSet bool
	if raw.DerivedBitsPerPixel != nil {
		format, _, err := session.ChromaFromBits(*raw.DerivedBitsPerPixel)
		if err != nil {
			return "", err.(*bizerror.Error).WithPath(path + ".derivedBitsPerPixel").WithScreen(screenID)
		}
		bitsFormat = format
		bitsSet = true
	}
	switch {
	case enumSet && bitsSet && enumFormat != bitsFormat:
		return "", bizerror.New(bizerror.ESchemaConflict, "planar enum and derived bit count identify different chroma formats").
			WithPath(path).WithScreen(screenID).
			WithDetail("enum_format", enumFormat).WithDetail("bits_format", bitsFormat)
	case enumSet:
		return enumFormat, nil
	case bitsSet:
		return bitsFormat, nil
	default:
		return "", bizerror.New(bizerror.EChromaEnumInvalid, "chroma planar format or derived bits is required").
			WithPath(path).WithScreen(screenID)
	}
}
