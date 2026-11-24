package scene

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"diffrender/errs"
)

type rawObject map[string]json.RawMessage

// LoadFile reads and decodes a versioned scene file.
func LoadFile(path string) (*Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeDecodeIO, path, "", "cannot read scene")
	}
	return Load(data, path)
}

// Load decodes one of the three supported scene schemas into the canonical
// WorkingVersion model. SourceVersion and upgrade trace are retained.
func Load(data []byte, file string) (*Scene, error) {
	var top rawObject
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, errs.New(errs.CodeVersion, file, "$", "invalid scene JSON: %v", err)
	}
	versionField, version, ok := detectVersion(top)
	if !ok {
		return nil, errs.New(errs.CodeVersion, file, "schema_version", "missing schema version (accepted keys: schema_version, schema, schemaVersion)")
	}
	switch version {
	case Version1:
		s, err := loadV1(top, file)
		if err != nil {
			return nil, err
		}
		s.SourceVersion = Version1
		return s, nil
	case Version2:
		s, err := loadV2(top, file)
		if err != nil {
			return nil, err
		}
		s.SourceVersion = Version2
		return s, nil
	case Version3:
		s, err := loadV3(top, file)
		if err != nil {
			return nil, err
		}
		s.SourceVersion = Version3
		return s, nil
	default:
		return nil, errs.New(errs.CodeVersion, file, versionField, "unsupported schema version %q (supported: 1.0, 2.0, 3.0)", version)
	}
}

func detectVersion(top rawObject) (string, string, bool) {
	keys := []string{"schema_version", "schema", "schemaVersion"}
	var foundField, foundValue string
	for _, key := range keys {
		raw, ok := top[key]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return key, "", false
		}
		if foundField != "" && foundValue != v {
			return foundField + "|" + key, "", false
		}
		foundField, foundValue = key, v
	}
	return foundField, foundValue, foundField != ""
}

func loadV1(top rawObject, file string) (*Scene, error) {
	id, err := reqString(top, "scene_id", "scene_id", file)
	if err != nil {
		return nil, err
	}
	objects, err := reqArray(top, "objects", "objects", file)
	if err != nil {
		return nil, err
	}
	lightsRaw, err := reqArray(top, "lights", "lights", file)
	if err != nil {
		return nil, err
	}
	camsRaw, err := reqArray(top, "cameras", "cameras", file)
	if err != nil {
		return nil, err
	}
	s := &Scene{
		ID:             id,
		SourceVersion:  Version1,
		WorkingVersion: WorkingVersion,
		Settings: Settings{
			ShadowSoftness: DefaultShadowSoftness,
			ToneMapping:    DefaultToneMapping,
		},
	}
	for i, raw := range objects {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("objects[%d]", i), "expected object")
		}
		typeName, err := reqString(obj, "type", fmt.Sprintf("objects[%d].type", i), file)
		if err != nil {
			return nil, err
		}
		switch typeName {
		case "sphere":
			path := fmt.Sprintf("objects[%d]", i)
			sid, err := reqString(obj, "id", path+".id", file)
			if err != nil {
				return nil, err
			}
			center, err := reqVec3Array(obj, "center", path+".center", file)
			if err != nil {
				return nil, err
			}
			radius, err := reqFloat(obj, "radius", path+".radius", file)
			if err != nil {
				return nil, err
			}
			albedo, err := reqVec3Array(obj, "albedo", path+".albedo", file)
			if err != nil {
				return nil, err
			}
			s.Spheres = append(s.Spheres, Sphere{ID: sid, Center: center, Radius: radius, Albedo: albedo})
			s.Trace = append(s.Trace, UpgradeTrace{
				Field:         fmt.Sprintf("geometry.spheres[%d].material.clearCoat", len(s.Spheres)-1),
				OldValue:      "<missing>",
				DefaultValue:  0.0,
				DefaultSource: "schema-v3-default",
				FromVersion:   Version1,
				ToVersion:     Version3,
			})
		case "plane":
			path := fmt.Sprintf("objects[%d]", i)
			pid, err := reqString(obj, "id", path+".id", file)
			if err != nil {
				return nil, err
			}
			normal, err := reqVec3Array(obj, "normal", path+".normal", file)
			if err != nil {
				return nil, err
			}
			offset, err := reqFloat(obj, "offset", path+".offset", file)
			if err != nil {
				return nil, err
			}
			albedo, err := reqVec3Array(obj, "albedo", path+".albedo", file)
			if err != nil {
				return nil, err
			}
			s.Planes = append(s.Planes, Plane{ID: pid, Normal: normal, Offset: offset, Albedo: albedo})
			s.Trace = append(s.Trace, UpgradeTrace{
				Field:         fmt.Sprintf("geometry.planes[%d].material.clearCoat", len(s.Planes)-1),
				OldValue:      "<missing>",
				DefaultValue:  0.0,
				DefaultSource: "schema-v3-default",
				FromVersion:   Version1,
				ToVersion:     Version3,
			})
		default:
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("objects[%d].type", i), "unsupported object type %q", typeName)
		}
	}
	for i, raw := range lightsRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("lights[%d]", i), "expected light object")
		}
		path := fmt.Sprintf("lights[%d]", i)
		kind, err := reqString(obj, "kind", path+".kind", file)
		if err != nil {
			return nil, err
		}
		if kind != "rectangle" {
			return nil, errs.New(errs.CodeVersion, file, path+".kind", "unsupported light kind %q", kind)
		}
		lid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		center, err := reqVec3Array(obj, "center", path+".center", file)
		if err != nil {
			return nil, err
		}
		normal, err := reqVec3Array(obj, "normal", path+".normal", file)
		if err != nil {
			return nil, err
		}
		up, err := reqVec3Array(obj, "up", path+".up", file)
		if err != nil {
			return nil, err
		}
		hw, err := reqFloat(obj, "half_width", path+".half_width", file)
		if err != nil {
			return nil, err
		}
		hh, err := reqFloat(obj, "half_height", path+".half_height", file)
		if err != nil {
			return nil, err
		}
		power, err := reqFloat(obj, "radiant_power_w", path+".radiant_power_w", file)
		if err != nil {
			return nil, err
		}
		color, err := reqVec3Array(obj, "color", path+".color", file)
		if err != nil {
			return nil, err
		}
		s.Lights = append(s.Lights, AreaLight{ID: lid, Center: center, Normal: normal, Up: up, HalfWidth: hw, HalfHeight: hh, RadiantPowerW: power, Color: color})
	}
	for i, raw := range camsRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("cameras[%d]", i), "expected camera object")
		}
		path := fmt.Sprintf("cameras[%d]", i)
		cid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		eye, err := reqVec3Array(obj, "eye", path+".eye", file)
		if err != nil {
			return nil, err
		}
		lookAt, err := reqVec3Array(obj, "look_at", path+".look_at", file)
		if err != nil {
			return nil, err
		}
		up, err := reqVec3Array(obj, "up", path+".up", file)
		if err != nil {
			return nil, err
		}
		fovDeg, err := reqFloat(obj, "fov_deg", path+".fov_deg", file)
		if err != nil {
			return nil, err
		}
		width, err := reqInt(obj, "width", path+".width", file)
		if err != nil {
			return nil, err
		}
		height, err := reqInt(obj, "height", path+".height", file)
		if err != nil {
			return nil, err
		}
		s.Cameras = append(s.Cameras, Camera{ID: cid, Eye: eye, LookAt: lookAt, Up: up, FovYRad: fovDeg * math.Pi / 180, Width: width, Height: height})
	}
	s.Trace = append(s.Trace,
		UpgradeTrace{
			Field: "render.shadowSoftness", OldValue: "<missing>", DefaultValue: DefaultShadowSoftness,
			DefaultSource: "schema-v2-default", FromVersion: Version1, ToVersion: Version2,
		},
		UpgradeTrace{
			Field: "render.toneMapping", OldValue: "<missing>", DefaultValue: DefaultToneMapping,
			DefaultSource: "schema-v3-default", FromVersion: Version2, ToVersion: Version3,
		},
	)
	return s, normalizeAndValidate(s, file)
}

func loadV2(top rawObject, file string) (*Scene, error) {
	assetRaw, err := reqObject(top, "asset", "asset", file)
	if err != nil {
		return nil, err
	}
	id, err := reqString(assetRaw, "id", "asset.id", file)
	if err != nil {
		return nil, err
	}
	geom, err := reqObject(top, "geometry", "geometry", file)
	if err != nil {
		return nil, err
	}
	spheresRaw, err := reqArray(geom, "spheres", "geometry.spheres", file)
	if err != nil {
		return nil, err
	}
	planesRaw, err := reqArray(geom, "planes", "geometry.planes", file)
	if err != nil {
		return nil, err
	}
	lightsRaw, err := reqArray(top, "lighting", "lighting", file)
	if err != nil {
		return nil, err
	}
	camsRaw, err := reqArray(top, "cameras", "cameras", file)
	if err != nil {
		return nil, err
	}
	s := &Scene{
		ID:             id,
		SourceVersion:  Version2,
		WorkingVersion: WorkingVersion,
		Settings:       Settings{ShadowSoftness: DefaultShadowSoftness, ToneMapping: DefaultToneMapping},
	}
	if renderRaw, ok := top["render"]; ok {
		var render rawObject
		if err := json.Unmarshal(renderRaw, &render); err != nil {
			return nil, errs.New(errs.CodeVersion, file, "render", "expected object")
		}
		if v, ok := render["shadow_softness"]; ok {
			if err := json.Unmarshal(v, &s.Settings.ShadowSoftness); err != nil {
				return nil, errs.New(errs.CodeVersion, file, "render.shadow_softness", "expected number")
			}
		}
	}
	for i, raw := range spheresRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("geometry.spheres[%d]", i), "expected sphere object")
		}
		path := fmt.Sprintf("geometry.spheres[%d]", i)
		sid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		center, err := reqVec3Object(obj, "position", path+".position", file)
		if err != nil {
			return nil, err
		}
		diameter, err := reqFloat(obj, "diameter", path+".diameter", file)
		if err != nil {
			return nil, err
		}
		albedo, err := reqVec3Object(obj, "base_color", path+".base_color", file)
		if err != nil {
			return nil, err
		}
		s.Spheres = append(s.Spheres, Sphere{ID: sid, Center: center, Radius: diameter / 2, Albedo: albedo})
		s.Trace = append(s.Trace, UpgradeTrace{
			Field:         fmt.Sprintf("geometry.spheres[%d].material.clearCoat", i),
			OldValue:      "<missing>",
			DefaultValue:  0.0,
			DefaultSource: "schema-v3-default",
			FromVersion:   Version2,
			ToVersion:     Version3,
		})
	}
	for i, raw := range planesRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("geometry.planes[%d]", i), "expected plane object")
		}
		path := fmt.Sprintf("geometry.planes[%d]", i)
		pid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		normal, err := reqVec3Array(obj, "normal_xyz", path+".normal_xyz", file)
		if err != nil {
			return nil, err
		}
		offset, err := reqFloat(obj, "distance_from_origin", path+".distance_from_origin", file)
		if err != nil {
			return nil, err
		}
		albedo, err := reqVec3Object(obj, "base_color", path+".base_color", file)
		if err != nil {
			return nil, err
		}
		s.Planes = append(s.Planes, Plane{ID: pid, Normal: normal, Offset: offset, Albedo: albedo})
		s.Trace = append(s.Trace, UpgradeTrace{
			Field:         fmt.Sprintf("geometry.planes[%d].material.clearCoat", i),
			OldValue:      "<missing>",
			DefaultValue:  0.0,
			DefaultSource: "schema-v3-default",
			FromVersion:   Version2,
			ToVersion:     Version3,
		})
	}
	for i, raw := range lightsRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("lighting[%d]", i), "expected light object")
		}
		path := fmt.Sprintf("lighting[%d]", i)
		lid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		shape, err := reqObject(obj, "shape", path+".shape", file)
		if err != nil {
			return nil, err
		}
		kind, err := reqString(shape, "type", path+".shape.type", file)
		if err != nil {
			return nil, err
		}
		if kind != "rectangle" {
			return nil, errs.New(errs.CodeVersion, file, path+".shape.type", "unsupported light shape %q", kind)
		}
		center, err := reqVec3Object(shape, "center", path+".shape.center", file)
		if err != nil {
			return nil, err
		}
		normal, err := reqVec3Array(shape, "normal", path+".shape.normal", file)
		if err != nil {
			return nil, err
		}
		up, err := reqVec3Array(shape, "up", path+".shape.up", file)
		if err != nil {
			return nil, err
		}
		size, err := reqFloatArray(shape, "half_size", path+".shape.half_size", file, 2)
		if err != nil {
			return nil, err
		}
		powerMW, err := reqFloat(obj, "radiant_power_mw", path+".radiant_power_mw", file)
		if err != nil {
			return nil, err
		}
		color, err := reqVec3Object(obj, "color", path+".color", file)
		if err != nil {
			return nil, err
		}
		s.Lights = append(s.Lights, AreaLight{ID: lid, Center: center, Normal: normal, Up: up, HalfWidth: size[0], HalfHeight: size[1], RadiantPowerW: powerMW / 1000, Color: color})
	}
	for i, raw := range camsRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("cameras[%d]", i), "expected camera object")
		}
		path := fmt.Sprintf("cameras[%d]", i)
		cid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		matrix, err := reqFloatArray(obj, "world_to_camera", path+".world_to_camera", file, 16)
		if err != nil {
			return nil, err
		}
		fov, err := reqFloat(obj, "vertical_fov_rad", path+".vertical_fov_rad", file)
		if err != nil {
			return nil, err
		}
		res, err := reqFloatArray(obj, "resolution", path+".resolution", file, 2)
		if err != nil {
			return nil, err
		}
		width, err := exactInt(res[0], path+".resolution[0]", file)
		if err != nil {
			return nil, err
		}
		height, err := exactInt(res[1], path+".resolution[1]", file)
		if err != nil {
			return nil, err
		}
		distance := 1.0
		if v, ok := obj["target_distance_m"]; ok {
			if err := json.Unmarshal(v, &distance); err != nil {
				return nil, errs.New(errs.CodeVersion, file, path+".target_distance_m", "expected number")
			}
		}
		cam, err := CameraFromWorldToCamera(cid, matrix, fov, width, height, distance)
		if err != nil {
			return nil, withFile(err, file)
		}
		s.Cameras = append(s.Cameras, cam)
	}
	s.Trace = append(s.Trace, UpgradeTrace{
		Field: "render.toneMapping", OldValue: "<missing>", DefaultValue: DefaultToneMapping,
		DefaultSource: "schema-v3-default", FromVersion: Version2, ToVersion: Version3,
	})
	return s, normalizeAndValidate(s, file)
}

func loadV3(top rawObject, file string) (*Scene, error) {
	id, err := reqString(top, "assetId", "assetId", file)
	if err != nil {
		return nil, err
	}
	nodes, err := reqObject(top, "nodes", "nodes", file)
	if err != nil {
		return nil, err
	}
	spheresRaw, err := reqArray(nodes, "spheres", "nodes.spheres", file)
	if err != nil {
		return nil, err
	}
	planesRaw, err := reqArray(nodes, "planes", "nodes.planes", file)
	if err != nil {
		return nil, err
	}
	lightsRaw, err := reqArray(nodes, "areaLights", "nodes.areaLights", file)
	if err != nil {
		return nil, err
	}
	camsRaw, err := reqArray(top, "cameras", "cameras", file)
	if err != nil {
		return nil, err
	}
	s := &Scene{
		ID:             id,
		SourceVersion:  Version3,
		WorkingVersion: WorkingVersion,
		Settings:       Settings{ShadowSoftness: DefaultShadowSoftness, ToneMapping: DefaultToneMapping},
	}
	if renderRaw, ok := top["render"]; ok {
		var render rawObject
		if err := json.Unmarshal(renderRaw, &render); err != nil {
			return nil, errs.New(errs.CodeVersion, file, "render", "expected object")
		}
		if v, ok := render["shadowSoftness"]; ok {
			if err := json.Unmarshal(v, &s.Settings.ShadowSoftness); err != nil {
				return nil, errs.New(errs.CodeVersion, file, "render.shadowSoftness", "expected number")
			}
		}
		if v, ok := render["toneMapping"]; ok {
			if err := json.Unmarshal(v, &s.Settings.ToneMapping); err != nil {
				return nil, errs.New(errs.CodeVersion, file, "render.toneMapping", "expected string")
			}
		}
	}
	for i, raw := range spheresRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("nodes.spheres[%d]", i), "expected sphere object")
		}
		path := fmt.Sprintf("nodes.spheres[%d]", i)
		sid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		center, err := reqVec3Array(obj, "centerXYZ", path+".centerXYZ", file)
		if err != nil {
			return nil, err
		}
		radius, err := reqFloat(obj, "radiusMeters", path+".radiusMeters", file)
		if err != nil {
			return nil, err
		}
		material, err := reqObject(obj, "material", path+".material", file)
		if err != nil {
			return nil, err
		}
		albedo, err := reqVec3Array(material, "baseColor", path+".material.baseColor", file)
		if err != nil {
			return nil, err
		}
		coat := 0.0
		if v, ok := material["clearCoat"]; ok {
			if err := json.Unmarshal(v, &coat); err != nil {
				return nil, errs.New(errs.CodeVersion, file, path+".material.clearCoat", "expected number")
			}
		}
		s.Spheres = append(s.Spheres, Sphere{ID: sid, Center: center, Radius: radius, Albedo: albedo, Coat: coat})
	}
	for i, raw := range planesRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("nodes.planes[%d]", i), "expected plane object")
		}
		path := fmt.Sprintf("nodes.planes[%d]", i)
		pid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		normal, err := reqVec3Array(obj, "normalXYZ", path+".normalXYZ", file)
		if err != nil {
			return nil, err
		}
		offset, err := reqFloat(obj, "offsetMeters", path+".offsetMeters", file)
		if err != nil {
			return nil, err
		}
		material, err := reqObject(obj, "material", path+".material", file)
		if err != nil {
			return nil, err
		}
		albedo, err := reqVec3Array(material, "baseColor", path+".material.baseColor", file)
		if err != nil {
			return nil, err
		}
		coat := 0.0
		if v, ok := material["clearCoat"]; ok {
			if err := json.Unmarshal(v, &coat); err != nil {
				return nil, errs.New(errs.CodeVersion, file, path+".material.clearCoat", "expected number")
			}
		}
		s.Planes = append(s.Planes, Plane{ID: pid, Normal: normal, Offset: offset, Albedo: albedo, Coat: coat})
	}
	for i, raw := range lightsRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("nodes.areaLights[%d]", i), "expected light object")
		}
		path := fmt.Sprintf("nodes.areaLights[%d]", i)
		lid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		center, err := reqVec3Array(obj, "centerXYZ", path+".centerXYZ", file)
		if err != nil {
			return nil, err
		}
		normal, err := reqVec3Array(obj, "normalXYZ", path+".normalXYZ", file)
		if err != nil {
			return nil, err
		}
		up, err := reqVec3Array(obj, "upXYZ", path+".upXYZ", file)
		if err != nil {
			return nil, err
		}
		size, err := reqFloatArray(obj, "sizeMeters", path+".sizeMeters", file, 2)
		if err != nil {
			return nil, err
		}
		flux, err := reqFloat(obj, "luminousFluxLm", path+".luminousFluxLm", file)
		if err != nil {
			return nil, err
		}
		color, err := reqVec3Array(obj, "colorLinear", path+".colorLinear", file)
		if err != nil {
			return nil, err
		}
		s.Lights = append(s.Lights, AreaLight{
			ID: lid, Center: center, Normal: normal, Up: up,
			HalfWidth: size[0] / 2, HalfHeight: size[1] / 2,
			RadiantPowerW: flux / LuminousEfficacy, Color: color,
		})
	}
	for i, raw := range camsRaw {
		var obj rawObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errs.New(errs.CodeVersion, file, fmt.Sprintf("cameras[%d]", i), "expected camera object")
		}
		path := fmt.Sprintf("cameras[%d]", i)
		cid, err := reqString(obj, "id", path+".id", file)
		if err != nil {
			return nil, err
		}
		eye, err := reqVec3Array(obj, "positionXYZ", path+".positionXYZ", file)
		if err != nil {
			return nil, err
		}
		quat, err := reqFloatArray(obj, "orientationQuat", path+".orientationQuat", file, 4)
		if err != nil {
			return nil, err
		}
		fovDeg, err := reqFloat(obj, "verticalFovDeg", path+".verticalFovDeg", file)
		if err != nil {
			return nil, err
		}
		film, err := reqObject(obj, "film", path+".film", file)
		if err != nil {
			return nil, err
		}
		width, err := reqInt(film, "widthPx", path+".film.widthPx", file)
		if err != nil {
			return nil, err
		}
		height, err := reqInt(film, "heightPx", path+".film.heightPx", file)
		if err != nil {
			return nil, err
		}
		distance := 1.0
		if v, ok := obj["lookAtDistanceMeters"]; ok {
			if err := json.Unmarshal(v, &distance); err != nil {
				return nil, errs.New(errs.CodeVersion, file, path+".lookAtDistanceMeters", "expected number")
			}
		}
		c2w, err := QuaternionToCameraToWorld(quat)
		if err != nil {
			return nil, withFile(err, file)
		}
		c2w[3], c2w[7], c2w[11] = eye.X, eye.Y, eye.Z
		cam, err := CameraFromCameraToWorld(cid, c2w, fovDeg*math.Pi/180, width, height, distance)
		if err != nil {
			return nil, withFile(err, file)
		}
		s.Cameras = append(s.Cameras, cam)
	}
	return s, normalizeAndValidate(s, file)
}

func normalizeAndValidate(s *Scene, file string) error {
	for i := range s.Planes {
		n, ok := s.Planes[i].Normal.Normalized()
		if !ok {
			return errs.New(errs.CodeParam, file, fmt.Sprintf("planes[%d].normal", i), "must be non-zero and finite")
		}
		length := s.Planes[i].Normal.Length()
		if math.Abs(length-1) > 1e-12 {
			s.Planes[i].Normal = n
			s.Planes[i].Offset /= length
		}
	}
	for i := range s.Lights {
		n, ok := s.Lights[i].Normal.Normalized()
		if !ok {
			return errs.New(errs.CodeParam, file, fmt.Sprintf("lights[%d].normal", i), "must be non-zero and finite")
		}
		s.Lights[i].Normal = n
		u, ok := s.Lights[i].Up.Normalized()
		if !ok {
			return errs.New(errs.CodeParam, file, fmt.Sprintf("lights[%d].up", i), "must be non-zero and finite")
		}
		s.Lights[i].Up = u
		if math.Abs(n.Dot(u)) > 0.999999 {
			return errs.New(errs.CodeParam, file, fmt.Sprintf("lights[%d].up", i), "must not be parallel to normal")
		}
	}
	return Validate(s, file)
}

// Validate checks geometry, material, light, camera, and renderer bounds.
func Validate(s *Scene, file string) error {
	if s == nil {
		return errs.New(errs.CodeVersion, file, "$", "scene is nil")
	}
	if strings.TrimSpace(s.ID) == "" {
		return errs.New(errs.CodeVersion, file, "asset.id", "required scene id is empty")
	}
	if !containsVersion(s.SourceVersion) {
		return errs.New(errs.CodeVersion, file, "schema_version", "unsupported source version %q", s.SourceVersion)
	}
	if s.WorkingVersion != WorkingVersion {
		return errs.New(errs.CodeVersion, file, "workingVersion", "unsupported internal version %q", s.WorkingVersion)
	}
	if len(s.Spheres) == 0 {
		return errs.New(errs.CodeVersion, file, "geometry.spheres", "at least one sphere is required")
	}
	if len(s.Planes) == 0 {
		return errs.New(errs.CodeVersion, file, "geometry.planes", "at least one plane is required")
	}
	if len(s.Lights) == 0 {
		return errs.New(errs.CodeVersion, file, "lighting", "at least one rectangle light is required")
	}
	if len(s.Cameras) == 0 {
		return errs.New(errs.CodeVersion, file, "cameras", "at least one camera is required")
	}
	if !isFinite(s.Settings.ShadowSoftness) || s.Settings.ShadowSoftness < 0 {
		return errs.New(errs.CodeParam, file, "render.shadowSoftness", "must be finite and >= 0")
	}
	if s.Settings.ToneMapping != "linear" && s.Settings.ToneMapping != "srgb" {
		return errs.New(errs.CodeParam, file, "render.toneMapping", "unsupported tone mapping %q", s.Settings.ToneMapping)
	}
	ids := make(map[string]string)
	for i, obj := range s.Spheres {
		path := fmt.Sprintf("spheres[%d]", i)
		if strings.TrimSpace(obj.ID) == "" {
			return errs.New(errs.CodeVersion, file, path+".id", "required id is empty")
		}
		if prev, ok := ids[obj.ID]; ok {
			return errs.New(errs.CodeVersion, file, path+".id", "duplicate id %q (already used by %s)", obj.ID, prev)
		}
		ids[obj.ID] = path
		if !obj.Center.IsFinite() {
			return errs.New(errs.CodeParam, file, path+".center", "must be finite")
		}
		if !isFinite(obj.Radius) || obj.Radius <= 1e-8 {
			return errs.New(errs.CodeParam, file, path+".radius", "must be finite and > 1e-8")
		}
		if err := validateAlbedo(obj.Albedo, file, path+".albedo"); err != nil {
			return err
		}
		if !isFinite(obj.Coat) || obj.Coat < 0 || obj.Coat > 1 {
			return errs.New(errs.CodeParam, file, path+".coat", "must be finite and in [0,1]")
		}
	}
	for i, obj := range s.Planes {
		path := fmt.Sprintf("planes[%d]", i)
		if strings.TrimSpace(obj.ID) == "" {
			return errs.New(errs.CodeVersion, file, path+".id", "required id is empty")
		}
		if prev, ok := ids[obj.ID]; ok {
			return errs.New(errs.CodeVersion, file, path+".id", "duplicate id %q (already used by %s)", obj.ID, prev)
		}
		ids[obj.ID] = path
		if !obj.Normal.IsFinite() || obj.Normal.LengthSquared() <= 1e-20 {
			return errs.New(errs.CodeParam, file, path+".normal", "must be non-zero and finite")
		}
		if !isFinite(obj.Offset) {
			return errs.New(errs.CodeParam, file, path+".offset", "must be finite")
		}
		if err := validateAlbedo(obj.Albedo, file, path+".albedo"); err != nil {
			return err
		}
		if !isFinite(obj.Coat) || obj.Coat < 0 || obj.Coat > 1 {
			return errs.New(errs.CodeParam, file, path+".coat", "must be finite and in [0,1]")
		}
	}
	for i, obj := range s.Lights {
		path := fmt.Sprintf("lights[%d]", i)
		if strings.TrimSpace(obj.ID) == "" {
			return errs.New(errs.CodeVersion, file, path+".id", "required id is empty")
		}
		if prev, ok := ids[obj.ID]; ok {
			return errs.New(errs.CodeVersion, file, path+".id", "duplicate id %q (already used by %s)", obj.ID, prev)
		}
		ids[obj.ID] = path
		if !obj.Center.IsFinite() || !obj.Normal.IsFinite() || !obj.Up.IsFinite() {
			return errs.New(errs.CodeParam, file, path, "light transform must be finite")
		}
		if !isFinite(obj.HalfWidth) || obj.HalfWidth <= 0 || !isFinite(obj.HalfHeight) || obj.HalfHeight <= 0 {
			return errs.New(errs.CodeParam, file, path+".half_size", "half extents must be finite and > 0")
		}
		if !isFinite(obj.RadiantPowerW) || obj.RadiantPowerW <= 0 {
			return errs.New(errs.CodeParam, file, path+".radiantPowerW", "must be finite and > 0")
		}
		if err := validateAlbedo(obj.Color, file, path+".color"); err != nil {
			return err
		}
	}
	for i, cam := range s.Cameras {
		path := fmt.Sprintf("cameras[%d]", i)
		if strings.TrimSpace(cam.ID) == "" {
			return errs.New(errs.CodeVersion, file, path+".id", "required id is empty")
		}
		if prev, ok := ids[cam.ID]; ok {
			return errs.New(errs.CodeVersion, file, path+".id", "duplicate id %q (already used by %s)", cam.ID, prev)
		}
		ids[cam.ID] = path
		if !cam.Eye.IsFinite() || !cam.LookAt.IsFinite() || !cam.Up.IsFinite() {
			return errs.New(errs.CodeParam, file, path, "camera transform must be finite")
		}
		if !isFinite(cam.FovYRad) || cam.FovYRad <= 1e-6 || cam.FovYRad >= math.Pi-1e-6 {
			return errs.New(errs.CodeParam, file, path+".fovYRad", "must be finite and in (0, pi)")
		}
		if cam.Width <= 0 || cam.Height <= 0 || cam.Width > 4096 || cam.Height > 4096 {
			return errs.New(errs.CodeParam, file, path+".resolution", "width and height must be in [1,4096]")
		}
		if _, _, _, err := CameraBasis(cam); err != nil {
			return withFile(err, file)
		}
	}
	return nil
}

func validateAlbedo(v Vec3, file, path string) error {
	if !v.IsFinite() || v.X < 0 || v.X > 1 || v.Y < 0 || v.Y > 1 || v.Z < 0 || v.Z > 1 {
		return errs.New(errs.CodeParam, file, path, "each channel must be finite and in [0,1]")
	}
	return nil
}

func containsVersion(v string) bool {
	for _, supported := range SupportedVersions {
		if v == supported {
			return true
		}
	}
	return false
}

func reqRaw(obj rawObject, key, path, file string) (json.RawMessage, error) {
	v, ok := obj[key]
	if !ok || len(v) == 0 || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return nil, errs.New(errs.CodeVersion, file, path, "required field is missing")
	}
	return v, nil
}

func reqString(obj rawObject, key, path, file string) (string, error) {
	raw, err := reqRaw(obj, key, path, file)
	if err != nil {
		return "", err
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", errs.New(errs.CodeVersion, file, path, "expected string")
	}
	if strings.TrimSpace(out) == "" {
		return "", errs.New(errs.CodeVersion, file, path, "required string is empty")
	}
	return out, nil
}

func reqFloat(obj rawObject, key, path, file string) (float64, error) {
	raw, err := reqRaw(obj, key, path, file)
	if err != nil {
		return 0, err
	}
	var out float64
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, errs.New(errs.CodeVersion, file, path, "expected number")
	}
	if !isFinite(out) {
		return 0, errs.New(errs.CodeVersion, file, path, "number is not finite")
	}
	return out, nil
}

func reqInt(obj rawObject, key, path, file string) (int, error) {
	v, err := reqFloat(obj, key, path, file)
	if err != nil {
		return 0, err
	}
	return exactInt(v, path, file)
}

func exactInt(v float64, path, file string) (int, error) {
	if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
		return 0, errs.New(errs.CodeVersion, file, path, "expected an integer")
	}
	return int(v), nil
}

func reqObject(obj rawObject, key, path, file string) (rawObject, error) {
	raw, err := reqRaw(obj, key, path, file)
	if err != nil {
		return nil, err
	}
	var out rawObject
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errs.New(errs.CodeVersion, file, path, "expected object")
	}
	return out, nil
}

func reqArray(obj rawObject, key, path, file string) ([]json.RawMessage, error) {
	raw, err := reqRaw(obj, key, path, file)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errs.New(errs.CodeVersion, file, path, "expected array")
	}
	return out, nil
}

func reqFloatArray(obj rawObject, key, path, file string, n int) ([]float64, error) {
	raw, err := reqRaw(obj, key, path, file)
	if err != nil {
		return nil, err
	}
	var vals []float64
	if err := json.Unmarshal(raw, &vals); err != nil {
		return nil, errs.New(errs.CodeVersion, file, path, "expected numeric array")
	}
	if len(vals) != n {
		return nil, errs.New(errs.CodeVersion, file, path, "expected %d values, got %d", n, len(vals))
	}
	for _, v := range vals {
		if !isFinite(v) {
			return nil, errs.New(errs.CodeVersion, file, path, "array contains a non-finite value")
		}
	}
	return vals, nil
}

func reqVec3Array(obj rawObject, key, path, file string) (Vec3, error) {
	vals, err := reqFloatArray(obj, key, path, file, 3)
	if err != nil {
		return Vec3{}, err
	}
	return Vec3{vals[0], vals[1], vals[2]}, nil
}

func reqVec3Object(obj rawObject, key, path, file string) (Vec3, error) {
	v, err := reqObject(obj, key, path, file)
	if err != nil {
		return Vec3{}, err
	}
	x, err := reqFloat(v, "x", path+".x", file)
	if err != nil {
		return Vec3{}, err
	}
	y, err := reqFloat(v, "y", path+".y", file)
	if err != nil {
		return Vec3{}, err
	}
	z, err := reqFloat(v, "z", path+".z", file)
	if err != nil {
		return Vec3{}, err
	}
	return Vec3{x, y, z}, nil
}

func withFile(err error, file string) error {
	e, ok := errs.As(err)
	if !ok {
		return errs.Wrap(err, errs.CodeVersion, file, "", "")
	}
	clone := *e
	clone.File = file
	return &clone
}
