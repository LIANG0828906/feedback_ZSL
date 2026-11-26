package workbench

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/xerr"
)

//go:embed web/index.html
var indexHTML []byte

type apiResponse struct {
	OK    bool `json:"ok"`
	Data  any  `json:"data,omitempty"`
	Error any  `json:"error,omitempty"`
}

func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"status": "ready", "service": "mesh-policy-workbench"})
	})
	mux.HandleFunc("GET /api/current", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, svc.Current())
	})
	mux.HandleFunc("GET /api/equivalence", func(w http.ResponseWriter, r *http.Request) {
		rep, err := svc.Equivalence()
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, rep)
	})
	mux.HandleFunc("POST /api/load", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version string `json:"version"`
			Workers int    `json:"workers"`
			Buffer  int    `json:"buffer_per_ns"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Version == "" {
			req.Version = "v1"
		}
		rep, err := svc.LoadVersion(req.Version, engine.RunOptions{Workers: req.Workers, BufferPerNS: req.Buffer})
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, rep)
	})
	mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) {
		var req engine.RunOptions
		if !decode(w, r, &req) {
			return
		}
		rep, err := svc.Run(req)
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, rep)
	})
	mux.HandleFunc("POST /api/edit", func(w http.ResponseWriter, r *http.Request) {
		var req engine.EditRequest
		if !decode(w, r, &req) {
			return
		}
		rep, err := svc.Edit(req)
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, rep)
	})
	mux.HandleFunc("POST /api/replace", func(w http.ResponseWriter, r *http.Request) {
		var req engine.ReplaceRequest
		if !decode(w, r, &req) {
			return
		}
		result, report, err := svc.Replace(req)
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, map[string]any{"replacement": result, "report": report})
	})
	mux.HandleFunc("POST /api/checkpoint/save", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Seq int64 `json:"seq"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cp, err := svc.SaveCheckpoint(req.Seq)
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, cp)
	})
	mux.HandleFunc("POST /api/checkpoint/restore", func(w http.ResponseWriter, r *http.Request) {
		rep, err := svc.RestoreCheckpoint()
		if err != nil {
			writeBizError(w, err)
			return
		}
		writeOK(w, rep)
	})
	mux.HandleFunc("POST /api/inject", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if !decode(w, r, &req) {
			return
		}
		data, err := svc.Inject(req.Type, req.Payload)
		if err != nil {
			writeBizErrorWithData(w, err, data)
			return
		}
		writeOK(w, data)
	})
	return loggingMiddleware(mux)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == nil {
		writeBizError(w, xerr.New(xerr.ERequestInvalid, "missing JSON body"))
		return false
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(target); err != nil {
		writeBizError(w, xerr.Wrap(xerr.ERequestInvalid, "invalid JSON request", err))
		return false
	}
	return true
}

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Data: data})
}

func writeBizError(w http.ResponseWriter, err error) {
	writeBizErrorWithData(w, err, nil)
}

func writeBizErrorWithData(w http.ResponseWriter, err error, data any) {
	status := http.StatusBadRequest
	code := xerr.CodeOf(err)
	switch code {
	case xerr.EBackpressure, xerr.EWorkerBudgetExhausted, xerr.ENamespaceBufferFull:
		status = http.StatusTooManyRequests
	case xerr.ECheckpointVersionMismatch:
		status = http.StatusConflict
	case xerr.EPolicyNotFound, xerr.EWorkloadMissing:
		status = http.StatusNotFound
	case xerr.ECheckpointOpen, xerr.ECheckpointCorrupt, xerr.ECheckpointTruncated:
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{OK: false, Data: nonNilData(data), Error: errorBody(err)})
}

func nonNilData(data any) any {
	if data == nil {
		return nil
	}
	return data
}

func errorBody(err error) any {
	if biz, ok := xerr.As(err); ok {
		return map[string]any{"code": biz.Code, "message": biz.Message, "context": biz.Context}
	}
	return map[string]any{"code": xerr.CodeOf(err), "message": err.Error()}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
