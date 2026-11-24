package httpserver

import (
	"encoding/json"
	"io/fs"
	"net/http"

	"volumelab/pkg/model"
	"volumelab/pkg/service"
)

type invokeEnvelope struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

func Handler(svc *service.Service, assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Invoke(r.Context(), "status", ""))
	})
	mux.HandleFunc("/api/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		var envelope invokeEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			writeJSON(w, http.StatusBadRequest, service.APIResult{
				OK:    false,
				Error: &model.AppError{Code: "E_ARG", Message: "invalid invoke envelope"},
			})
			return
		}
		payload := "{}"
		if len(envelope.Payload) > 0 {
			payload = string(envelope.Payload)
		}
		writeJSON(w, http.StatusOK, svc.Invoke(r.Context(), envelope.Action, payload))
	})
	if assets != nil {
		mux.Handle("/", http.FileServer(http.FS(assets)))
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
