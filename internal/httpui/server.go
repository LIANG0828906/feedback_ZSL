package httpui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"occlusionlab/internal/model"
	"occlusionlab/internal/workbench"
)

type Server struct {
	service  *workbench.Service
	assetDir string
	http     *http.Server
}

type envelope struct {
	Data  any             `json:"data,omitempty"`
	Error *model.AppError `json:"error,omitempty"`
}

func New(service *workbench.Service, address, assetDir string) *Server {
	s := &Server{service: service, assetDir: assetDir}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bootstrap", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.Bootstrap()
	}))
	mux.HandleFunc("/api/dashboard", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.Dashboard()
	}))
	mux.HandleFunc("/api/generate", s.handle(func(r *http.Request) (any, *model.AppError) {
		var opts workbench.GenerateOptions
		if err := decodeBody(r, &opts); err != nil {
			return nil, err
		}
		return s.service.Regenerate(opts)
	}))
	mux.HandleFunc("/api/params", s.handle(func(r *http.Request) (any, *model.AppError) {
		var opts workbench.RuntimeOptions
		if err := decodeBody(r, &opts); err != nil {
			return nil, err
		}
		return s.service.UpdateParams(opts)
	}))
	mux.HandleFunc("/api/step", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.Step()
	}))
	mux.HandleFunc("/api/process-all", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.ProcessAll()
	}))
	mux.HandleFunc("/api/pause", s.handle(func(r *http.Request) (any, *model.AppError) {
		var body struct {
			Paused bool `json:"paused"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		return s.service.SetPaused(body.Paused)
	}))
	mux.HandleFunc("/api/retry", s.handle(func(r *http.Request) (any, *model.AppError) {
		var body struct {
			Window int `json:"window"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		return s.service.Retry(body.Window)
	}))
	mux.HandleFunc("/api/select", s.handle(func(r *http.Request) (any, *model.AppError) {
		var body struct {
			Window int `json:"window"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		return s.service.SelectWindow(body.Window)
	}))
	mux.HandleFunc("/api/camera", s.handle(func(r *http.Request) (any, *model.AppError) {
		var body struct {
			Window int     `json:"window"`
			DX     float64 `json:"dx"`
			DY     float64 `json:"dy"`
			DZ     float64 `json:"dz"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		return s.service.MoveCamera(body.Window, body.DX, body.DY, body.DZ)
	}))
	mux.HandleFunc("/api/validate", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.Validate()
	}))
	mux.HandleFunc("/api/reset", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.Reset()
	}))
	mux.HandleFunc("/api/export", s.handle(func(r *http.Request) (any, *model.AppError) {
		return s.service.ExportProject()
	}))
	mux.HandleFunc("/api/meta", s.handle(func(r *http.Request) (any, *model.AppError) {
		return map[string]any{
			"app_version": workbench.AppVersion,
			"data_dir":    s.service.DataDir(),
			"project_dir": s.service.ProjectDir(),
			"status":      s.service.StatusText(),
		}, nil
	}))
	mux.Handle("/", http.FileServer(http.Dir(assetDir)))
	handler := logging(mux)
	s.http = &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

func (s *Server) Serve(listener net.Listener) error {
	return s.http.Serve(listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handle(fn func(*http.Request) (any, *model.AppError)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(envelope{Error: &model.AppError{
				Code: model.CodeSchema, Message: "仅支持 GET/POST",
			}})
			return
		}
		data, appErr := fn(r)
		if appErr != nil {
			status := http.StatusBadRequest
			if appErr.Code == model.CodeInternal {
				status = http.StatusInternalServerError
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(envelope{Error: appErr})
			return
		}
		_ = json.NewEncoder(w).Encode(envelope{Data: data})
	}
}

func decodeBody(r *http.Request, out any) *model.AppError {
	if r.Body == nil {
		return model.SchemaError(0, "body", "请求体为空")
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return model.IOError("body", "无法读取请求体", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return model.SchemaError(0, "body", "请求体为空")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return model.SchemaError(0, "body", "请求体不是合法 JSON")
	}
	return nil
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fmt.Fprintf(os.Stderr, "%s %s\n", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func FindAssetDir() string {
	candidates := []string{
		"frontend/dist",
		filepath.Join("..", "frontend", "dist"),
	}
	for _, candidate := range candidates {
		if stat, err := os.Stat(candidate); err == nil && stat.IsDir() {
			return candidate
		}
	}
	return "frontend/dist"
}

func FriendlyListenError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
