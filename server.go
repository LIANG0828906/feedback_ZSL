package terrainbake

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type Server struct {
	demo    *Demo
	handler http.Handler
}

func NewServer(demo *Demo) (*Server, error) {
	if demo == nil {
		return nil, errSchema("demo", "nil demo")
	}
	index, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(index)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, demo.View())
	})
	mux.HandleFunc("/api/edit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		var edit Edit
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
			writeError(w, http.StatusBadRequest, errDecode("body", "invalid edit JSON: "+err.Error()))
			return
		}
		if edit.BlockID == "" || (edit.HeightDeltaMM == nil && edit.Budget == nil && edit.ToleranceUM == nil) {
			writeError(w, http.StatusBadRequest, errSchema("body", "block_id and at least one edit field are required"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		state, err := demo.ApplyEdit(ctx, edit)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("/api/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		report, err := demo.Verify(ctx)
		if err != nil {
			writeJSON(w, statusFor(err), map[string]any{
				"report": report,
				"error":  asError(err),
			})
			return
		}
		writeJSON(w, http.StatusOK, report)
	})
	mux.HandleFunc("/api/fault", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		var req struct {
			Kind    string `json:"kind"`
			BlockID string `json:"block_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errDecode("body", err.Error()))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		state, ferr := demo.InjectFault(ctx, req.Kind, req.BlockID)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"state": state,
			"error": ferr,
		})
	})
	mux.HandleFunc("/api/frame", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		t, _ := strconv.ParseInt(r.URL.Query().Get("t_ms"), 10, 64)
		writeJSON(w, http.StatusOK, CameraAt(demo.project, t))
	})
	mux.HandleFunc("/api/schema", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		version := strings.TrimPrefix(strings.ToLower(r.URL.Query().Get("v")), "v")
		n, err := strconv.Atoi(version)
		if err != nil || !IsKnownSchemaVersion(SchemaVersion(n)) {
			writeError(w, http.StatusBadRequest, errSchema("v", "supported versions are 1, 2 and 3"))
			return
		}
		catalog, err := SchemaCatalog(demo.project)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		data := catalog["v"+version]
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=terrain.v%s.json", version))
		_, _ = w.Write(data)
	})
	return &Server{demo: demo, handler: mux}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, &Error{Code: CodeSchema, Field: "method", Message: "method not allowed"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": asError(err)})
}

func statusFor(err error) int {
	e := asError(err)
	switch e.Code {
	case CodeSchema, CodeDecode, CodePara, CodeOvf:
		return http.StatusBadRequest
	case CodeAdj, CodeOver, CodeAttr, CodeDiff, CodeBlock:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
