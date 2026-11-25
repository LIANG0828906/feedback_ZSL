package dialogue

// CloneProject creates an isolated deep copy sufficient for edit/full
// comparison. Read-only truth and scene tree are shared.
func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Clips = make([]*Clip, 0, len(p.Clips))
	cp.Markers = append([]Marker(nil), p.Markers...)
	cp.Traces = append([]FieldTrace(nil), p.Traces...)
	cp.SceneTree = p.SceneTree
	for _, clip := range p.Clips {
		cc := *clip
		cc.Samples = append([]int16(nil), clip.Samples...)
		cc.ScenePath = append([]string(nil), clip.ScenePath...)
		cc.LoadTraces = append([]FieldTrace(nil), clip.LoadTraces...)
		cp.Clips = append(cp.Clips, &cc)
	}
	return &cp
}
