package diagnostic

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"ballistic/internal/kernel"
)

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	testCase := r.URL.Query().Get("case")
	result := InjectionResult{Case: testCase}

	var err error
	switch testCase {
	case "decode":
		result.ExpectedCode = string(kernel.CodeDecode)
		targetID := "wall_right"
		original := []byte(`"` + targetID + `"`)
		corrupt := []byte(`"` + targetID + `__corrupt`)
		bad := bytes.Replace(s.sample.Documents[kernel.SchemaV3], original, corrupt, 1)
		_, err = kernel.LoadDocument(bad)
		var decodeErr *kernel.Error
		if errors.As(err, &decodeErr) {
			decodeErr.ID = targetID
			decodeErr.Field = "$.obstacles[id=" + targetID + "]"
		}
	case "order":
		result.ExpectedCode = string(kernel.CodeOrder)
		truth := *s.sample.Truth
		truth.Samples = append([]kernel.TruthSample{}, s.sample.Truth.Samples...)
		swapped := false
		for i := 1; i+1 < len(truth.Samples); i++ {
			if truth.Samples[i].RequestID == truth.Samples[i+1].RequestID {
				truth.Samples[i].LogicalFrame, truth.Samples[i+1].LogicalFrame =
					truth.Samples[i+1].LogicalFrame, truth.Samples[i].LogicalFrame
				swapped = true
				break
			}
		}
		if !swapped {
			err = kernel.NewOrderError("$.truth.samples", "", "no swappable timeline samples")
		} else {
			err = kernel.ValidateTruthTimeline(&truth)
		}
	case "para":
		result.ExpectedCode = string(kernel.CodePara)
		_, err = s.wb.EditRestitution("wall_right", kernel.CoeffScale+2345)
	case "block":
		result.ExpectedCode = string(kernel.CodeBlock)
		tileID := "tx07_ty04"
		_, err = kernel.RenderTile(s.wb.Mapping, tileID, map[string]bool{tileID: true})
	case "schema":
		result.ExpectedCode = string(kernel.CodeSchema)
		var doc map[string]json.RawMessage
		if jsonErr := json.Unmarshal(s.sample.Documents[kernel.SchemaV3], &doc); jsonErr != nil {
			err = jsonErr
			break
		}
		delete(doc, "level_id")
		data, _ := json.Marshal(doc)
		_, err = kernel.LoadDocument(data)
	default:
		http.Error(w, "unknown injection case", http.StatusBadRequest)
		return
	}

	var kernelErr *kernel.Error
	if errors.As(err, &kernelErr) {
		issue := kernelErr.Issue
		result.Issue = &issue
		result.OK = string(kernelErr.Code) == result.ExpectedCode
	} else if err != nil {
		result.Message = err.Error()
	} else {
		result.Message = "injection did not fail"
	}
	s.injection = &result
	s.writeJSON(w, http.StatusOK, result)
}
