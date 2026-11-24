package spectralrender

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
)

func DecodeJSONFile(path string) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, decodeError(path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, decodeError(path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, decodeError(path, err)
	}
	return v, nil
}

func ParseSceneJSON(data []byte) (*Scene, *Error) {
	var v any
	dec := json.NewDecoder(bytesReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, decodeError("scene", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, decodeError("scene", err)
	}
	return parseScene(v)
}

func bytesReader(data []byte) io.Reader {
	return &byteReader{data: data}
}

type byteReader struct {
	data []byte
	off  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func LoadScene(path string) (*Scene, string, error) {
	v, err := DecodeJSONFile(path)
	if err != nil {
		return nil, "", err
	}
	scene, perr := parseScene(v)
	if perr != nil {
		return nil, "", perr
	}
	return scene, HashScene(scene), nil
}

func parseScene(v any) (*Scene, *Error) {
	root, e := asObject(v, "scene")
	if e != nil {
		return nil, e
	}
	s := &Scene{}
	var err *Error

	if s.SchemaVersion, err = requiredInt(root, "schema_version", "scene"); err != nil {
		return nil, err
	}
	if s.SchemaVersion != sceneSchema {
		return nil, schemaError("scene.schema_version", fmt.Sprintf("unsupported schema version %d", s.SchemaVersion))
	}
	if s.Seed, err = requiredInt64(root, "seed", "scene"); err != nil {
		return nil, err
	}
	if s.Width, err = requiredInt(root, "width", "scene"); err != nil {
		return nil, err
	}
	if s.Height, err = requiredInt(root, "height", "scene"); err != nil {
		return nil, err
	}
	if s.Width <= 0 || s.Width > 8192 {
		return nil, schemaError("scene.width", "must be in [1,8192]")
	}
	if s.Height <= 0 || s.Height > 8192 {
		return nil, schemaError("scene.height", "must be in [1,8192]")
	}

	q, e := required(root, "quantization", "scene")
	if e != nil {
		return nil, e
	}
	if s.Quantization, err = parseQuantization(q, "scene.quantization"); err != nil {
		return nil, err
	}
	sp, e := required(root, "spectrum", "scene")
	if e != nil {
		return nil, e
	}
	if s.Spectrum, err = parseSpectrum(sp, "scene.spectrum"); err != nil {
		return nil, err
	}
	n := len(s.Spectrum.Wavelengths)

	mats, e := requiredArray(root, "materials", "scene")
	if e != nil {
		return nil, e
	}
	if len(mats) == 0 {
		return nil, schemaError("scene.materials", "must contain at least one material")
	}
	seenMaterials := make(map[string]bool)
	s.Materials = make([]Material, 0, len(mats))
	for i, raw := range mats {
		m, me := parseMaterial(raw, fmt.Sprintf("scene.materials[%d]", i), n)
		if me != nil {
			return nil, me
		}
		if seenMaterials[m.ID] {
			return nil, schemaError(fmt.Sprintf("scene.materials[%d].id", i), "duplicate material id")
		}
		seenMaterials[m.ID] = true
		s.Materials = append(s.Materials, m)
	}

	meshes, e := requiredArray(root, "meshes", "scene")
	if e != nil {
		return nil, e
	}
	if len(meshes) == 0 {
		return nil, schemaError("scene.meshes", "must contain at least one mesh")
	}
	seenMeshes := make(map[string]bool)
	s.Meshes = make([]Mesh, 0, len(meshes))
	for i, raw := range meshes {
		mesh, me := parseMesh(raw, fmt.Sprintf("scene.meshes[%d]", i), s.Quantization, seenMaterials)
		if me != nil {
			return nil, me
		}
		if seenMeshes[mesh.ID] {
			return nil, schemaError(fmt.Sprintf("scene.meshes[%d].id", i), "duplicate mesh id")
		}
		seenMeshes[mesh.ID] = true
		s.Meshes = append(s.Meshes, mesh)
	}

	lights, e := requiredArray(root, "lights", "scene")
	if e != nil {
		return nil, e
	}
	if len(lights) == 0 {
		return nil, schemaError("scene.lights", "must contain at least one light")
	}
	seenLights := make(map[string]bool)
	s.Lights = make([]Light, 0, len(lights))
	for i, raw := range lights {
		l, le := parseLight(raw, fmt.Sprintf("scene.lights[%d]", i), n)
		if le != nil {
			return nil, le
		}
		if seenLights[l.ID] {
			return nil, schemaError(fmt.Sprintf("scene.lights[%d].id", i), "duplicate light id")
		}
		seenLights[l.ID] = true
		s.Lights = append(s.Lights, l)
	}

	if bg, ok := root["background_linear_rgb"]; ok {
		rgb, re := parseFloatArray(bg, "scene.background_linear_rgb", 3)
		if re != nil {
			return nil, re
		}
		s.BackgroundRGB = RGB{rgb[0], rgb[1], rgb[2]}
		if !s.BackgroundRGB.IsFinite() || s.BackgroundRGB.Min() < 0 {
			return nil, schemaError("scene.background_linear_rgb", "must be finite and non-negative")
		}
	}
	return s, nil
}

func parseQuantization(v any, path string) (Quantization, *Error) {
	m, e := asObject(v, path)
	if e != nil {
		return Quantization{}, e
	}
	q := Quantization{}
	var err *Error
	if q.VertexBits, err = requiredInt(m, "vertex_bits", path); err != nil {
		return q, err
	}
	uv, err := requiredInt64(m, "vertex_unit", path)
	if err != nil {
		return q, err
	}
	q.VertexUnit = uv
	if q.CoefficientBits, err = requiredInt(m, "coefficient_bits", path); err != nil {
		return q, err
	}
	uc, err := requiredInt64(m, "coefficient_unit", path)
	if err != nil {
		return q, err
	}
	q.CoefficientUnit = uc
	if q.VertexBits < 1 || q.VertexBits > 62 {
		return q, schemaError(path+".vertex_bits", "must be in [1,62]")
	}
	if q.CoefficientBits < 1 || q.CoefficientBits > 62 {
		return q, schemaError(path+".coefficient_bits", "must be in [1,62]")
	}
	if q.VertexUnit <= 0 {
		return q, schemaError(path+".vertex_unit", "must be positive")
	}
	if q.CoefficientUnit <= 0 {
		return q, schemaError(path+".coefficient_unit", "must be positive")
	}
	return q, nil
}

func parseSpectrum(v any, path string) (Spectrum, *Error) {
	m, e := asObject(v, path)
	if e != nil {
		return Spectrum{}, e
	}
	sp := Spectrum{}
	var err *Error
	if sp.StartNM, err = requiredFloat(m, "start_nm", path); err != nil {
		return sp, err
	}
	if sp.EndNM, err = requiredFloat(m, "end_nm", path); err != nil {
		return sp, err
	}
	if sp.StepNM, err = requiredFloat(m, "step_nm", path); err != nil {
		return sp, err
	}
	if !isFinite(sp.StartNM) || !isFinite(sp.EndNM) || !isFinite(sp.StepNM) {
		return sp, schemaError(path, "wavelength bounds must be finite")
	}
	if sp.EndNM <= sp.StartNM || sp.StepNM <= 0 {
		return sp, schemaError(path, "end_nm must exceed start_nm and step_nm must be positive")
	}
	w, e := requiredArray(m, "wavelengths", path)
	if e != nil {
		return sp, e
	}
	if len(w) < 2 {
		return sp, schemaError(path+".wavelengths", "must contain at least two samples")
	}
	sp.Wavelengths = make([]float64, len(w))
	for i, raw := range w {
		x, xe := asFloat(raw, fmt.Sprintf("%s.wavelengths[%d]", path, i))
		if xe != nil {
			return sp, xe
		}
		if !isFinite(x) {
			return sp, schemaError(fmt.Sprintf("%s.wavelengths[%d]", path, i), "must be finite")
		}
		if i > 0 && x <= sp.Wavelengths[i-1] {
			return sp, schemaError(fmt.Sprintf("%s.wavelengths[%d]", path, i), "wavelength samples must be strictly increasing")
		}
		sp.Wavelengths[i] = x
	}
	if math.Abs(sp.Wavelengths[0]-sp.StartNM) > 1e-9 || math.Abs(sp.Wavelengths[len(w)-1]-sp.EndNM) > 1e-9 {
		return sp, schemaError(path+".wavelengths", "first/last sample must equal start_nm/end_nm")
	}

	cmfRaw, e := required(m, "cmf", path)
	if e != nil {
		return sp, e
	}
	cmfObj, e := asObject(cmfRaw, path+".cmf")
	if e != nil {
		return sp, e
	}
	x, e := requiredArray(cmfObj, "x", path+".cmf")
	if e != nil {
		return sp, e
	}
	y, e := requiredArray(cmfObj, "y", path+".cmf")
	if e != nil {
		return sp, e
	}
	z, e := requiredArray(cmfObj, "z", path+".cmf")
	if e != nil {
		return sp, e
	}
	cmfArrays := []struct {
		name string
		arr  []any
	}{{"x", x}, {"y", y}, {"z", z}}
	for _, item := range cmfArrays {
		if len(item.arr) != len(w) {
			return sp, schemaError(fmt.Sprintf("%s.cmf.%s", path, item.name),
				fmt.Sprintf("CMF length %d does not match wavelength sample count %d", len(item.arr), len(w)))
		}
	}
	sp.CMF.X, e = parseFloatArray(x, path+".cmf.x", len(w))
	if e != nil {
		return sp, e
	}
	sp.CMF.Y, e = parseFloatArray(y, path+".cmf.y", len(w))
	if e != nil {
		return sp, e
	}
	sp.CMF.Z, e = parseFloatArray(z, path+".cmf.z", len(w))
	if e != nil {
		return sp, e
	}
	for i := 0; i < len(w); i++ {
		if !isFinite(sp.CMF.X[i]) || !isFinite(sp.CMF.Y[i]) || !isFinite(sp.CMF.Z[i]) {
			return sp, schemaError(fmt.Sprintf("%s.cmf[%d]", path, i), "CMF values must be finite")
		}
	}

	wp, e := requiredArray(m, "white_point_xyz", path)
	if e != nil {
		return sp, e
	}
	if len(wp) != 3 {
		return sp, schemaError(path+".white_point_xyz", "must contain three values")
	}
	for i, raw := range wp {
		v, ve := asFloat(raw, fmt.Sprintf("%s.white_point_xyz[%d]", path, i))
		if ve != nil {
			return sp, ve
		}
		if !isFinite(v) || v <= 0 {
			return sp, schemaError(fmt.Sprintf("%s.white_point_xyz[%d]", path, i), "must be finite and positive")
		}
		sp.WhitePoint[i] = v
	}

	mRaw, e := required(m, "xyz_to_rgb", path)
	if e != nil {
		return sp, e
	}
	rows, e := asArray(mRaw, path+".xyz_to_rgb")
	if e != nil {
		return sp, e
	}
	if len(rows) != 3 {
		return sp, schemaError(path+".xyz_to_rgb", "must be a 3x3 matrix")
	}
	for i := range rows {
		vals, ae := parseFloatArray(rows[i], fmt.Sprintf("%s.xyz_to_rgb[%d]", path, i), 3)
		if ae != nil {
			return sp, ae
		}
		sp.XYZToRGB[i] = [3]float64{vals[0], vals[1], vals[2]}
	}

	if sp.TargetGamut, err = requiredString(m, "target_gamut", path); err != nil {
		return sp, err
	}
	if sp.TargetGamut == "" {
		return sp, schemaError(path+".target_gamut", "must not be empty")
	}
	return sp, nil
}

func parseMaterial(v any, path string, sampleCount int) (Material, *Error) {
	m, e := asObject(v, path)
	if e != nil {
		return Material{}, e
	}
	mat := Material{}
	var err *Error
	if mat.ID, err = requiredString(m, "id", path); err != nil {
		return mat, err
	}
	if mat.ID == "" {
		return mat, schemaError(path+".id", "must not be empty")
	}
	if mat.CoefficientBits, err = requiredInt(m, "coefficient_bits", path); err != nil {
		return mat, err
	}
	unit, err := requiredInt64(m, "coefficient_unit", path)
	if err != nil {
		return mat, err
	}
	mat.CoefficientUnit = unit
	if mat.CoefficientBits < 1 || mat.CoefficientBits > 62 || mat.CoefficientUnit <= 0 {
		return mat, schemaError(path+".coefficient_unit", "coefficient_bits/coefficient_unit must be positive and consistent")
	}
	basis, e := requiredArray(m, "basis", path)
	if e != nil {
		return mat, e
	}
	if len(basis) == 0 {
		return mat, schemaError(path+".basis", "must contain at least one basis function")
	}
	mat.Basis = make([]BasisFunction, 0, len(basis))
	for i, raw := range basis {
		bp := fmt.Sprintf("%s.basis[%d]", path, i)
		bo, be := asObject(raw, bp)
		if be != nil {
			return mat, be
		}
		name, ne := requiredString(bo, "name", bp)
		if ne != nil {
			return mat, ne
		}
		vals, ve := requiredArray(bo, "values", bp)
		if ve != nil {
			return mat, ve
		}
		if len(vals) != sampleCount {
			return mat, schemaError(bp+".values",
				fmt.Sprintf("basis length %d does not match wavelength sample count %d", len(vals), sampleCount))
		}
		bv, ve := parseFloatArray(vals, bp+".values", sampleCount)
		if ve != nil {
			return mat, ve
		}
		for j, x := range bv {
			if !isFinite(x) {
				return mat, schemaError(fmt.Sprintf("%s.values[%d]", bp, j), "must be finite")
			}
		}
		mat.Basis = append(mat.Basis, BasisFunction{Name: name, Values: bv})
	}
	coeffs, e := requiredArray(m, "coefficients", path)
	if e != nil {
		return mat, e
	}
	if len(coeffs) != len(mat.Basis) {
		return mat, schemaError(path+".coefficients",
			fmt.Sprintf("coefficient count %d does not match basis count %d", len(coeffs), len(mat.Basis)))
	}
	mat.Coefficients = make([]int64, len(coeffs))
	for i, raw := range coeffs {
		c, ce := asInt64(raw, fmt.Sprintf("%s.coefficients[%d]", path, i))
		if ce != nil {
			return mat, ce
		}
		mat.Coefficients[i] = c
	}
	return mat, nil
}

func parseMesh(v any, path string, q Quantization, materialIDs map[string]bool) (Mesh, *Error) {
	m, e := asObject(v, path)
	if e != nil {
		return Mesh{}, e
	}
	mesh := Mesh{}
	var err *Error
	if mesh.ID, err = requiredString(m, "id", path); err != nil {
		return mesh, err
	}
	if mesh.ID == "" {
		return mesh, schemaError(path+".id", "must not be empty")
	}
	if mesh.MaterialID, err = requiredString(m, "material_id", path); err != nil {
		return mesh, err
	}
	if !materialIDs[mesh.MaterialID] {
		return mesh, schemaError(path+".material_id", "references an unknown material")
	}
	verts, e := requiredArray(m, "vertices", path)
	if e != nil {
		return mesh, e
	}
	if len(verts) < 3 {
		return mesh, schemaError(path+".vertices", "must contain at least three vertices")
	}
	mesh.Vertices = make([]Vertex, 0, len(verts))
	for i, raw := range verts {
		vp := fmt.Sprintf("%s.vertices[%d]", path, i)
		vo, ve := asObject(raw, vp)
		if ve != nil {
			return mesh, ve
		}
		var vert Vertex
		pos, pe := requiredArray(vo, "position", vp)
		if pe != nil {
			return mesh, pe
		}
		posv, pe := parseFloatArray(pos, vp+".position", 3)
		if pe != nil {
			return mesh, pe
		}
		vert.Position = [3]float64{posv[0], posv[1], posv[2]}
		nrm, ne := requiredArray(vo, "normal", vp)
		if ne != nil {
			return mesh, ne
		}
		nrmv, ne := parseFloatArray(nrm, vp+".normal", 3)
		if ne != nil {
			return mesh, ne
		}
		vert.Normal = [3]float64{nrmv[0], nrmv[1], nrmv[2]}
		albedo, ae := requiredArray(vo, "albedo", vp)
		if ae != nil {
			return mesh, ae
		}
		if len(albedo) != 3 {
			return mesh, schemaError(vp+".albedo", "must contain three integer components")
		}
		for c, raw := range albedo {
			x, xe := asInt64(raw, fmt.Sprintf("%s.albedo[%d]", vp, c))
			if xe != nil {
				return mesh, xe
			}
			if x < 0 || x > q.VertexUnit {
				return mesh, schemaError(fmt.Sprintf("%s.albedo[%d]", vp, c),
					fmt.Sprintf("value %d is outside declared vertex quantization unit [0,%d]", x, q.VertexUnit))
			}
			vert.Albedo[c] = x
		}
		if vert.Roughness, err = requiredInt64(vo, "roughness", vp); err != nil {
			return mesh, err
		}
		if vert.Roughness < 0 || vert.Roughness > q.VertexUnit {
			return mesh, schemaError(vp+".roughness",
				fmt.Sprintf("value %d is outside declared vertex quantization unit [0,%d]", vert.Roughness, q.VertexUnit))
		}
		if vert.Metallic, err = requiredInt64(vo, "metallic", vp); err != nil {
			return mesh, err
		}
		if vert.Metallic < 0 || vert.Metallic > q.VertexUnit {
			return mesh, schemaError(vp+".metallic",
				fmt.Sprintf("value %d is outside declared vertex quantization unit [0,%d]", vert.Metallic, q.VertexUnit))
		}
		mesh.Vertices = append(mesh.Vertices, vert)
	}
	tris, e := requiredArray(m, "triangles", path)
	if e != nil {
		return mesh, e
	}
	if len(tris) == 0 {
		return mesh, schemaError(path+".triangles", "must contain at least one triangle")
	}
	mesh.Triangles = make([][3]int, 0, len(tris))
	for i, raw := range tris {
		tp := fmt.Sprintf("%s.triangles[%d]", path, i)
		vals, ae := asArray(raw, tp)
		if ae != nil {
			return mesh, ae
		}
		if len(vals) != 3 {
			return mesh, schemaError(tp, "triangle must contain three indices")
		}
		var tri [3]int
		for j, rv := range vals {
			idx, ie := asInt64(rv, fmt.Sprintf("%s[%d]", tp, j))
			if ie != nil {
				return mesh, ie
			}
			if idx < 0 || idx >= int64(len(mesh.Vertices)) {
				return mesh, schemaError(fmt.Sprintf("%s[%d]", tp, j), "vertex index is out of range")
			}
			tri[j] = int(idx)
		}
		if tri[0] == tri[1] || tri[1] == tri[2] || tri[0] == tri[2] {
			return mesh, schemaError(tp, "triangle indices must be distinct")
		}
		mesh.Triangles = append(mesh.Triangles, tri)
	}
	return mesh, nil
}

func parseLight(v any, path string, sampleCount int) (Light, *Error) {
	m, e := asObject(v, path)
	if e != nil {
		return Light{}, e
	}
	l := Light{}
	var err *Error
	if l.ID, err = requiredString(m, "id", path); err != nil {
		return l, err
	}
	if l.ID == "" {
		return l, schemaError(path+".id", "must not be empty")
	}
	d, e := requiredArray(m, "direction", path)
	if e != nil {
		return l, e
	}
	dv, e := parseFloatArray(d, path+".direction", 3)
	if e != nil {
		return l, e
	}
	l.Direction = [3]float64{dv[0], dv[1], dv[2]}
	if math.Sqrt(dot(l.Direction, l.Direction)) < 1e-12 {
		return l, schemaError(path+".direction", "direction must be non-zero")
	}
	intensity, e := requiredArray(m, "intensity", path)
	if e != nil {
		return l, e
	}
	if len(intensity) != sampleCount {
		return l, schemaError(path+".intensity",
			fmt.Sprintf("intensity length %d does not match wavelength sample count %d", len(intensity), sampleCount))
	}
	l.Intensity = make([]float64, len(intensity))
	for i, raw := range intensity {
		x, xe := asFloat(raw, fmt.Sprintf("%s.intensity[%d]", path, i))
		if xe != nil {
			return l, xe
		}
		if !isFinite(x) || x < 0 {
			return l, schemaError(fmt.Sprintf("%s.intensity[%d]", path, i), "must be finite and non-negative")
		}
		l.Intensity[i] = x
	}
	return l, nil
}

func LoadTruth(path string) (*TruthManifest, string, error) {
	v, err := DecodeJSONFile(path)
	if err != nil {
		return nil, "", err
	}
	t, perr := parseTruth(v)
	if perr != nil {
		return nil, "", perr
	}
	return t, HashTruth(t), nil
}

func parseTruth(v any) (*TruthManifest, *Error) {
	root, e := asObject(v, "truth")
	if e != nil {
		return nil, e
	}
	t := &TruthManifest{}
	var err *Error
	if t.SchemaVersion, err = requiredInt(root, "schema_version", "truth"); err != nil {
		return nil, err
	}
	if t.SchemaVersion != truthSchema {
		return nil, schemaError("truth.schema_version", fmt.Sprintf("unsupported truth schema version %d", t.SchemaVersion))
	}
	if t.SceneHash, err = requiredString(root, "scene_hash", "truth"); err != nil {
		return nil, err
	}
	if t.SceneHash == "" {
		return nil, schemaError("truth.scene_hash", "must not be empty")
	}
	refRaw, e := required(root, "reference", "truth")
	if e != nil {
		return nil, e
	}
	ref, e := asObject(refRaw, "truth.reference")
	if e != nil {
		return nil, e
	}
	if t.Reference.SampleCount, err = requiredInt(ref, "sample_count", "truth.reference"); err != nil {
		return nil, err
	}
	if t.Reference.Integration, err = requiredString(ref, "integration", "truth.reference"); err != nil {
		return nil, err
	}
	if t.Reference.Bounces, err = requiredInt(ref, "bounces", "truth.reference"); err != nil {
		return nil, err
	}
	if t.Reference.IndirectWeight, err = requiredFloat(ref, "indirect_weight", "truth.reference"); err != nil {
		return nil, err
	}
	pixels, e := requiredArray(root, "pixels", "truth")
	if e != nil {
		return nil, e
	}
	if len(pixels) == 0 {
		return nil, schemaError("truth.pixels", "must contain at least one truth pixel")
	}
	t.Pixels = make([]TruthPixel, 0, len(pixels))
	for i, raw := range pixels {
		pp := fmt.Sprintf("truth.pixels[%d]", i)
		po, pe := asObject(raw, pp)
		if pe != nil {
			return nil, pe
		}
		var px TruthPixel
		if px.X, err = requiredInt(po, "x", pp); err != nil {
			return nil, err
		}
		if px.Y, err = requiredInt(po, "y", pp); err != nil {
			return nil, err
		}
		rgbRaw, re := requiredArray(po, "linear_rgb", pp)
		if re != nil {
			return nil, re
		}
		vals, re := parseFloatArray(rgbRaw, pp+".linear_rgb", 3)
		if re != nil {
			return nil, re
		}
		px.LinearRGB = RGB{vals[0], vals[1], vals[2]}
		if px.Tolerance, err = requiredFloat(po, "tolerance", pp); err != nil {
			return nil, err
		}
		if px.PeakRadiance, err = requiredFloat(po, "peak_radiance", pp); err != nil {
			return nil, err
		}
		if px.PeakTolerance, err = requiredFloat(po, "peak_tolerance", pp); err != nil {
			return nil, err
		}
		xyRaw, xe := requiredArray(po, "chromaticity_xy", pp)
		if xe != nil {
			return nil, xe
		}
		xy, xe := parseFloatArray(xyRaw, pp+".chromaticity_xy", 2)
		if xe != nil {
			return nil, xe
		}
		px.ChromaticityXY = [2]float64{xy[0], xy[1]}
		if px.ChromaticityBound, err = requiredFloat(po, "chromaticity_bound", pp); err != nil {
			return nil, err
		}
		if px.Tolerance < 0 || px.PeakTolerance < 0 || px.ChromaticityBound < 0 {
			return nil, schemaError(pp, "tolerances must be non-negative")
		}
		if !px.LinearRGB.IsFinite() || !isFinite(px.PeakRadiance) || !isFinite(px.ChromaticityXY[0]) || !isFinite(px.ChromaticityXY[1]) {
			return nil, schemaError(pp, "truth values must be finite")
		}
		t.Pixels = append(t.Pixels, px)
	}
	boundsRaw, e := required(root, "bounds", "truth")
	if e != nil {
		return nil, e
	}
	bo, e := asObject(boundsRaw, "truth.bounds")
	if e != nil {
		return nil, e
	}
	if t.Bounds.LinearRGBAbs, err = requiredFloat(bo, "linear_rgb_abs", "truth.bounds"); err != nil {
		return nil, err
	}
	if t.Bounds.PeakRadianceRel, err = requiredFloat(bo, "peak_radiance_rel", "truth.bounds"); err != nil {
		return nil, err
	}
	if t.Bounds.ChromaticityXY, err = requiredFloat(bo, "chromaticity_xy", "truth.bounds"); err != nil {
		return nil, err
	}
	if t.Bounds.LinearRGBAbs < 0 || t.Bounds.PeakRadianceRel < 0 || t.Bounds.ChromaticityXY < 0 {
		return nil, schemaError("truth.bounds", "bounds must be non-negative")
	}
	return t, nil
}

func ValidateTruthForScene(t *TruthManifest, scene *Scene, sceneHash string) *Error {
	if t.SceneHash != sceneHash {
		return schemaError("truth.scene_hash", "truth manifest does not belong to the loaded scene")
	}
	for i, px := range t.Pixels {
		if px.X < 0 || px.X >= scene.Width || px.Y < 0 || px.Y >= scene.Height {
			return schemaError(fmt.Sprintf("truth.pixels[%d]", i), "pixel coordinate is outside the scene")
		}
	}
	return nil
}

func asObject(v any, path string) (map[string]any, *Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaError(path, "expected object")
	}
	return m, nil
}

func asArray(v any, path string) ([]any, *Error) {
	a, ok := v.([]any)
	if !ok {
		return nil, schemaError(path, "expected array")
	}
	return a, nil
}

func required(m map[string]any, key, path string) (any, *Error) {
	v, ok := m[key]
	if !ok {
		p := path + "." + key
		return nil, schemaError(p, "missing required field")
	}
	return v, nil
}

func requiredArray(m map[string]any, key, path string) ([]any, *Error) {
	v, e := required(m, key, path)
	if e != nil {
		return nil, e
	}
	return asArray(v, path+"."+key)
}

func requiredString(m map[string]any, key, path string) (string, *Error) {
	v, e := required(m, key, path)
	if e != nil {
		return "", e
	}
	s, ok := v.(string)
	if !ok {
		return "", schemaError(path+"."+key, "expected string")
	}
	return s, nil
}

func requiredInt(m map[string]any, key, path string) (int, *Error) {
	v, e := required(m, key, path)
	if e != nil {
		return 0, e
	}
	x, ie := asInt64(v, path+"."+key)
	if ie != nil {
		return 0, ie
	}
	if x < math.MinInt32 || x > math.MaxInt32 {
		return 0, schemaError(path+"."+key, "integer is out of range")
	}
	return int(x), nil
}

func requiredInt64(m map[string]any, key, path string) (int64, *Error) {
	v, e := required(m, key, path)
	if e != nil {
		return 0, e
	}
	return asInt64(v, path+"."+key)
}

func requiredFloat(m map[string]any, key, path string) (float64, *Error) {
	v, e := required(m, key, path)
	if e != nil {
		return 0, e
	}
	return asFloat(v, path+"."+key)
}

func asInt64(v any, path string) (int64, *Error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaError(path, "expected integer")
	}
	s := n.String()
	x, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, schemaError(path, "expected an integer encoded without a fractional part or exponent")
	}
	return x, nil
}

func asFloat(v any, path string) (float64, *Error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaError(path, "expected number")
	}
	x, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || !isFinite(x) {
		return 0, schemaError(path, "expected finite number")
	}
	return x, nil
}

func parseFloatArray(v any, path string, want int) ([]float64, *Error) {
	a, e := asArray(v, path)
	if e != nil {
		return nil, e
	}
	if want >= 0 && len(a) != want {
		return nil, schemaError(path, fmt.Sprintf("expected %d values, got %d", want, len(a)))
	}
	out := make([]float64, len(a))
	for i, raw := range a {
		x, xe := asFloat(raw, fmt.Sprintf("%s[%d]", path, i))
		if xe != nil {
			return nil, xe
		}
		out[i] = x
	}
	return out, nil
}
