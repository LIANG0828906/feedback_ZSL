package scene

import (
	"encoding/json"
	"fmt"
	"math"

	"diffrender/errs"
)

// LossItem describes an unsupported field encountered during a downgrade.
type LossItem struct {
	Field         string `json:"field"`
	OriginalValue string `json:"originalValue"`
	Disposition   string `json:"disposition"`
}

// ExportOptions controls target version selection and explicit loss handling.
type ExportOptions struct {
	Version    string
	AllowLossy bool
}

// ExportResult contains deterministic bytes and any explicit downgrade report.
type ExportResult struct {
	Data   []byte
	Losses []LossItem
}

// Export serializes the canonical model into a supported target schema.
// Unsupported non-default fields produce E_LOSSY unless AllowLossy is true.
func Export(s *Scene, opts ExportOptions) (ExportResult, error) {
	if !containsVersion(opts.Version) {
		return ExportResult{}, errs.New(errs.CodeVersion, "<export>", "targetVersion", "unsupported target version %q", opts.Version)
	}
	if err := Validate(s, "<export>"); err != nil {
		return ExportResult{}, err
	}
	losses := collectLosses(s, opts.Version)
	if len(losses) > 0 && !opts.AllowLossy {
		e := errs.New(errs.CodeLossy, "<export>", "export", "%d field(s) require an explicit lossy downgrade", len(losses))
		e.Details = losses
		return ExportResult{Losses: losses}, e
	}

	var (
		doc any
		err error
	)
	switch opts.Version {
	case Version1:
		doc, err = exportV1(s, losses)
	case Version2:
		doc, err = exportV2(s, losses)
	case Version3:
		doc, err = exportV3(s)
	}
	if err != nil {
		return ExportResult{Losses: losses}, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ExportResult{Losses: losses}, errs.Wrap(err, errs.CodeConv, "<export>", "export", "cannot encode scene")
	}
	data = append(data, '\n')
	return ExportResult{Data: data, Losses: losses}, nil
}

func collectLosses(s *Scene, target string) []LossItem {
	var out []LossItem
	if target == Version1 || target == Version2 {
		for i, sp := range s.Spheres {
			if sp.Coat != 0 {
				out = append(out, LossItem{
					Field:         fmt.Sprintf("spheres[%d].coat", i),
					OriginalValue: fmt.Sprintf("%.17g", sp.Coat),
					Disposition:   "omitted; target schema has no clear-coat field",
				})
			}
		}
		for i, pl := range s.Planes {
			if pl.Coat != 0 {
				out = append(out, LossItem{
					Field:         fmt.Sprintf("planes[%d].coat", i),
					OriginalValue: fmt.Sprintf("%.17g", pl.Coat),
					Disposition:   "omitted; target schema has no clear-coat field",
				})
			}
		}
	}
	if target == Version1 || target == Version2 {
		if s.Settings.ToneMapping != DefaultToneMapping {
			out = append(out, LossItem{
				Field:         "settings.toneMapping",
				OriginalValue: fmt.Sprintf("%q", s.Settings.ToneMapping),
				Disposition:   "omitted; target schema has no toneMapping field",
			})
		}
	}
	if target == Version1 && s.Settings.ShadowSoftness != DefaultShadowSoftness {
		out = append(out, LossItem{
			Field:         "settings.shadowSoftness",
			OriginalValue: fmt.Sprintf("%.17g", s.Settings.ShadowSoftness),
			Disposition:   "omitted; target schema has no shadow-softness field",
		})
	}
	return out
}

type v1Object struct {
	Type   string    `json:"type"`
	ID     string    `json:"id"`
	Center []float64 `json:"center,omitempty"`
	Radius *float64  `json:"radius,omitempty"`
	Albedo []float64 `json:"albedo"`
	Normal []float64 `json:"normal,omitempty"`
	Offset *float64  `json:"offset,omitempty"`
}

type v1Light struct {
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	Center        []float64 `json:"center"`
	Normal        []float64 `json:"normal"`
	Up            []float64 `json:"up"`
	HalfWidth     float64   `json:"half_width"`
	HalfHeight    float64   `json:"half_height"`
	RadiantPowerW float64   `json:"radiant_power_w"`
	Color         []float64 `json:"color"`
}

type v1Camera struct {
	ID     string    `json:"id"`
	Eye    []float64 `json:"eye"`
	LookAt []float64 `json:"look_at"`
	Up     []float64 `json:"up"`
	FovDeg float64   `json:"fov_deg"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
}

type v1Document struct {
	SchemaVersion string     `json:"schema_version"`
	SceneID       string     `json:"scene_id"`
	Objects       []v1Object `json:"objects"`
	Lights        []v1Light  `json:"lights"`
	Cameras       []v1Camera `json:"cameras"`
}

func exportV1(s *Scene, _ []LossItem) (v1Document, error) {
	doc := v1Document{SchemaVersion: Version1, SceneID: s.ID}
	for _, sp := range s.Spheres {
		radius := sp.Radius
		doc.Objects = append(doc.Objects, v1Object{
			Type: "sphere", ID: sp.ID, Center: vecArray(sp.Center), Radius: &radius, Albedo: vecArray(sp.Albedo),
		})
	}
	for _, pl := range s.Planes {
		offset := pl.Offset
		doc.Objects = append(doc.Objects, v1Object{
			Type: "plane", ID: pl.ID, Normal: vecArray(pl.Normal), Offset: &offset, Albedo: vecArray(pl.Albedo),
		})
	}
	for _, li := range s.Lights {
		doc.Lights = append(doc.Lights, v1Light{
			Kind: "rectangle", ID: li.ID, Center: vecArray(li.Center),
			Normal: vecArray(li.Normal), Up: vecArray(li.Up),
			HalfWidth: li.HalfWidth, HalfHeight: li.HalfHeight,
			RadiantPowerW: li.RadiantPowerW, Color: vecArray(li.Color),
		})
	}
	for _, cam := range s.Cameras {
		doc.Cameras = append(doc.Cameras, v1Camera{
			ID: cam.ID, Eye: vecArray(cam.Eye), LookAt: vecArray(cam.LookAt), Up: vecArray(cam.Up),
			FovDeg: cam.FovYRad * 180 / math.Pi, Width: cam.Width, Height: cam.Height,
		})
	}
	return doc, nil
}

type v2Vec struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type v2Sphere struct {
	ID        string  `json:"id"`
	Position  v2Vec   `json:"position"`
	Diameter  float64 `json:"diameter"`
	BaseColor v2Vec   `json:"base_color"`
}

type v2Plane struct {
	ID                 string    `json:"id"`
	NormalXYZ          []float64 `json:"normal_xyz"`
	DistanceFromOrigin float64   `json:"distance_from_origin"`
	BaseColor          v2Vec     `json:"base_color"`
}

type v2Geometry struct {
	Spheres []v2Sphere `json:"spheres"`
	Planes  []v2Plane  `json:"planes"`
}

type v2Shape struct {
	Type     string    `json:"type"`
	Center   v2Vec     `json:"center"`
	Normal   []float64 `json:"normal"`
	Up       []float64 `json:"up"`
	HalfSize []float64 `json:"half_size"`
}

type v2Light struct {
	ID             string  `json:"id"`
	Shape          v2Shape `json:"shape"`
	RadiantPowerMW float64 `json:"radiant_power_mw"`
	Color          v2Vec   `json:"color"`
}

type v2Camera struct {
	ID              string    `json:"id"`
	WorldToCamera   []float64 `json:"world_to_camera"`
	VerticalFovRad  float64   `json:"vertical_fov_rad"`
	Resolution      []int     `json:"resolution"`
	TargetDistanceM float64   `json:"target_distance_m"`
}

type v2Render struct {
	ShadowSoftness float64 `json:"shadow_softness"`
}

type v2Document struct {
	Schema string `json:"schema"`
	Asset  struct {
		ID string `json:"id"`
	} `json:"asset"`
	Geometry v2Geometry `json:"geometry"`
	Lighting []v2Light  `json:"lighting"`
	Cameras  []v2Camera `json:"cameras"`
	Render   v2Render   `json:"render"`
}

func exportV2(s *Scene, _ []LossItem) (v2Document, error) {
	doc := v2Document{Schema: Version2}
	doc.Asset.ID = s.ID
	for _, sp := range s.Spheres {
		doc.Geometry.Spheres = append(doc.Geometry.Spheres, v2Sphere{
			ID: sp.ID, Position: v2Vec(sp.Center), Diameter: sp.Radius * 2, BaseColor: v2Vec(sp.Albedo),
		})
	}
	for _, pl := range s.Planes {
		doc.Geometry.Planes = append(doc.Geometry.Planes, v2Plane{
			ID: pl.ID, NormalXYZ: vecArray(pl.Normal), DistanceFromOrigin: pl.Offset, BaseColor: v2Vec(pl.Albedo),
		})
	}
	for _, li := range s.Lights {
		doc.Lighting = append(doc.Lighting, v2Light{
			ID: li.ID,
			Shape: v2Shape{
				Type: "rectangle", Center: v2Vec(li.Center), Normal: vecArray(li.Normal),
				Up: vecArray(li.Up), HalfSize: []float64{li.HalfWidth, li.HalfHeight},
			},
			RadiantPowerMW: li.RadiantPowerW * 1000,
			Color:          v2Vec(li.Color),
		})
	}
	for _, cam := range s.Cameras {
		m, err := WorldToCamera(cam)
		if err != nil {
			return doc, err
		}
		dist, err := lookAtDistance(cam)
		if err != nil {
			return doc, err
		}
		doc.Cameras = append(doc.Cameras, v2Camera{
			ID: cam.ID, WorldToCamera: matrixArray(m), VerticalFovRad: cam.FovYRad,
			Resolution: []int{cam.Width, cam.Height}, TargetDistanceM: dist,
		})
	}
	doc.Render.ShadowSoftness = s.Settings.ShadowSoftness
	return doc, nil
}

type v3Material struct {
	BaseColor []float64 `json:"baseColor"`
	ClearCoat float64   `json:"clearCoat"`
}

type v3Sphere struct {
	ID           string     `json:"id"`
	CenterXYZ    []float64  `json:"centerXYZ"`
	RadiusMeters float64    `json:"radiusMeters"`
	Material     v3Material `json:"material"`
}

type v3Plane struct {
	ID           string     `json:"id"`
	NormalXYZ    []float64  `json:"normalXYZ"`
	OffsetMeters float64    `json:"offsetMeters"`
	Material     v3Material `json:"material"`
}

type v3AreaLight struct {
	ID             string    `json:"id"`
	CenterXYZ      []float64 `json:"centerXYZ"`
	NormalXYZ      []float64 `json:"normalXYZ"`
	UpXYZ          []float64 `json:"upXYZ"`
	SizeMeters     []float64 `json:"sizeMeters"`
	LuminousFluxLm float64   `json:"luminousFluxLm"`
	ColorLinear    []float64 `json:"colorLinear"`
}

type v3Nodes struct {
	Spheres    []v3Sphere    `json:"spheres"`
	Planes     []v3Plane     `json:"planes"`
	AreaLights []v3AreaLight `json:"areaLights"`
}

type v3Film struct {
	WidthPx  int `json:"widthPx"`
	HeightPx int `json:"heightPx"`
}

type v3Camera struct {
	ID                   string    `json:"id"`
	PositionXYZ          []float64 `json:"positionXYZ"`
	OrientationQuat      []float64 `json:"orientationQuat"`
	VerticalFovDeg       float64   `json:"verticalFovDeg"`
	Film                 v3Film    `json:"film"`
	LookAtDistanceMeters float64   `json:"lookAtDistanceMeters"`
}

type v3Render struct {
	ShadowSoftness float64 `json:"shadowSoftness"`
	ToneMapping    string  `json:"toneMapping"`
}

type v3Document struct {
	SchemaVersion string     `json:"schemaVersion"`
	AssetID       string     `json:"assetId"`
	Nodes         v3Nodes    `json:"nodes"`
	Cameras       []v3Camera `json:"cameras"`
	Render        v3Render   `json:"render"`
}

func exportV3(s *Scene) (v3Document, error) {
	doc := v3Document{SchemaVersion: Version3, AssetID: s.ID}
	for _, sp := range s.Spheres {
		doc.Nodes.Spheres = append(doc.Nodes.Spheres, v3Sphere{
			ID: sp.ID, CenterXYZ: vecArray(sp.Center), RadiusMeters: sp.Radius,
			Material: v3Material{BaseColor: vecArray(sp.Albedo), ClearCoat: sp.Coat},
		})
	}
	for _, pl := range s.Planes {
		doc.Nodes.Planes = append(doc.Nodes.Planes, v3Plane{
			ID: pl.ID, NormalXYZ: vecArray(pl.Normal), OffsetMeters: pl.Offset,
			Material: v3Material{BaseColor: vecArray(pl.Albedo), ClearCoat: pl.Coat},
		})
	}
	for _, li := range s.Lights {
		doc.Nodes.AreaLights = append(doc.Nodes.AreaLights, v3AreaLight{
			ID: li.ID, CenterXYZ: vecArray(li.Center), NormalXYZ: vecArray(li.Normal), UpXYZ: vecArray(li.Up),
			SizeMeters:     []float64{li.HalfWidth * 2, li.HalfHeight * 2},
			LuminousFluxLm: li.RadiantPowerW * LuminousEfficacy,
			ColorLinear:    vecArray(li.Color),
		})
	}
	for _, cam := range s.Cameras {
		c2w, err := CameraToWorld(cam)
		if err != nil {
			return doc, err
		}
		q := MatrixToQuaternion(c2w)
		dist, err := lookAtDistance(cam)
		if err != nil {
			return doc, err
		}
		doc.Cameras = append(doc.Cameras, v3Camera{
			ID: cam.ID, PositionXYZ: vecArray(cam.Eye), OrientationQuat: quatArray(q),
			VerticalFovDeg:       cam.FovYRad * 180 / math.Pi,
			Film:                 v3Film{WidthPx: cam.Width, HeightPx: cam.Height},
			LookAtDistanceMeters: dist,
		})
	}
	doc.Render = v3Render{ShadowSoftness: s.Settings.ShadowSoftness, ToneMapping: s.Settings.ToneMapping}
	return doc, nil
}

func vecArray(v Vec3) []float64 { return []float64{v.X, v.Y, v.Z} }
func matrixArray(m [16]float64) []float64 {
	out := make([]float64, 16)
	copy(out, m[:])
	return out
}
func quatArray(q [4]float64) []float64 { return []float64{q[0], q[1], q[2], q[3]} }

func lookAtDistance(c Camera) (float64, error) {
	d := c.LookAt.Sub(c.Eye).Length()
	if !isFinite(d) || d <= 1e-12 {
		return 0, errs.New(errs.CodeParam, "<export>", "cameras."+c.ID+".look_at", "must define a positive finite look-at distance")
	}
	return d, nil
}
