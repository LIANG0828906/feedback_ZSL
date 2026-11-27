package volumefog

import (
	"encoding/json"
	"testing"
)

func sceneMap(t *testing.T, scene *Scene) map[string]any {
	t.Helper()
	data, err := MarshalSceneJSON(scene)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func loadMap(t *testing.T, m map[string]any) (*Scene, error) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return LoadSceneJSON(data)
}

func firstMedia(m map[string]any) map[string]any {
	return m["media"].([]any)[0].(map[string]any)
}

func TestSchemaValidationLocations(t *testing.T) {
	base := sceneMap(t, testScene(t))
	cases := []struct {
		name   string
		code   Code
		media  string
		field  string
		mutate func(map[string]any)
	}{
		{"missing media field", E_SCHEMA, "fog_a", "absorption_q", func(m map[string]any) {
			delete(firstMedia(m), "absorption_q")
		}},
		{"missing top field", E_SCHEMA, "", "seed", func(m map[string]any) { delete(m, "seed") }},
		{"inverted bounds", E_SCHEMA, "fog_a", "bounds", func(m map[string]any) {
			firstMedia(m)["min_q"] = []any{1e12, 0, 0}
		}},
		{"negative scattering", E_SCHEMA, "fog_a", "scattering_q", func(m map[string]any) {
			firstMedia(m)["scattering_q"] = -1.0
		}},
		{"camera order", E_ORDER, "camera[1]", "time_us", func(m map[string]any) {
			cams := m["cameras"].([]any)
			cams[1].(map[string]any)["time_us"] = cams[0].(map[string]any)["time_us"]
		}},
		{"density range", E_DENS, "fog_a", "density[0]", func(m map[string]any) {
			scene := testScene(t)
			scene.Media[0].Density[0] = 999
			fresh := sceneMap(t, scene)
			for k, v := range fresh {
				m[k] = v
			}
		}},
		{"corrupt payload", E_DECODE, "fog_a", "density_checksum", func(m map[string]any) {
			b64 := firstMedia(m)["density_b64"].(string)
			if b64[0] == 'A' {
				firstMedia(m)["density_b64"] = "B" + b64[1:]
			} else {
				firstMedia(m)["density_b64"] = "A" + b64[1:]
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			raw, _ := json.Marshal(base)
			_ = json.Unmarshal(raw, &m)
			tc.mutate(m)
			_, err := loadMap(t, m)
			if err == nil {
				t.Fatal("expected error")
			}
			fe, ok := err.(*FogError)
			if !ok {
				t.Fatalf("error type %T: %v", err, err)
			}
			if fe.Code != tc.code {
				t.Fatalf("code=%s want %s (%v)", fe.Code, tc.code, err)
			}
			if fe.MediaID != tc.media {
				t.Fatalf("media=%q want %q (%v)", fe.MediaID, tc.media, err)
			}
			if fe.Field != tc.field && tc.code != E_DECODE {
				t.Fatalf("field=%q want %q (%v)", fe.Field, tc.field, err)
			}
		})
	}
}
