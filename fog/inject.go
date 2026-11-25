package fog

import "context"

type InjectionKind string

const (
	InjectDecode  InjectionKind = "decode"
	InjectOrder   InjectionKind = "order"
	InjectDensity InjectionKind = "density"
	InjectBlock   InjectionKind = "block"
)

type InjectionReport struct {
	Kind      InjectionKind `json:"kind"`
	Code      ErrorCode     `json:"code"`
	Fault     *Fault        `json:"fault"`
	Frame     *Frame        `json:"frame,omitempty"`
	Surviving int           `json:"surviving_tiles,omitempty"`
	Failed    int           `json:"failed_tiles,omitempty"`
}

func RunInjection(scene *Scene, kind InjectionKind, failTile, failMedia string) *InjectionReport {
	cp := scene.Clone()
	switch kind {
	case InjectDecode:
		if len(cp.Media) > 0 && len(cp.Media[0].Density) > 0 {
			cp.Media[0].Density[0] = DensityMax + 7
		}
		if f := cp.Validate(); f != nil {
			return &InjectionReport{Kind: kind, Code: f.Code, Fault: f}
		}
	case InjectOrder:
		if len(cp.Cameras) >= 2 {
			cp.Cameras[1].LogicalTime = cp.Cameras[0].LogicalTime - 1
		}
		if f := cp.Validate(); f != nil {
			return &InjectionReport{Kind: kind, Code: f.Code, Fault: f}
		}
	case InjectDensity:
		if len(cp.Media) > 0 {
			cp.Media[0].DensityRes[0] = 65
		}
		if f := cp.Validate(); f != nil {
			return &InjectionReport{Kind: kind, Code: f.Code, Fault: f}
		}
	case InjectBlock:
		if failTile == "" {
			failTile = "tile-r00-c00"
		}
		if f := cp.Validate(); f != nil {
			return &InjectionReport{Kind: kind, Code: f.Code, Fault: f}
		}
		eng := NewEngine(cp, nil)
		frame, err := eng.Render(context.Background(), RenderOptions{
			Mode: RenderExact, FailTile: failTile, FailMediaID: failMedia,
		})
		rep := &InjectionReport{Kind: kind, Frame: frame}
		if frame != nil {
			rep.Surviving = len(frame.Tiles)
			rep.Failed = 1
		}
		if faults, ok := err.(FaultList); ok && len(faults) > 0 {
			rep.Code, rep.Fault = faults[0].Code, faults[0]
			return rep
		}
		if f, ok := FaultAs(err); ok {
			rep.Code, rep.Fault = f.Code, f
			return rep
		}
	}
	return &InjectionReport{Kind: kind, Code: EPara, Fault: &Fault{Code: EPara, Path: "injection", Message: "fault injection did not trigger"}}
}
