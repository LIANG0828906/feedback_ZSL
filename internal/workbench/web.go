package workbench

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mvccworkbench/internal/diagnostics"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
)

//go:embed web/index.html
var webFS embed.FS

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/batches", s.handleBatches)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/document", s.handleDocument)
	mux.HandleFunc("/api/inject", s.handleInject)
	mux.HandleFunc("/api/crash-resume", s.handleCrashResume)
	mux.Handle("/", http.FileServer(http.FS(mustSubFS())))
	return mux
}

func mustSubFS() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return sub
}

func ListenAndServe(addr string, service *Service) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return server.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDomainError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := domain.CodeOf(err)
	if code == domain.ECkptOpen || code == domain.ECkptWrite || code == domain.ECkptRead {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error(), "code": code})
}

func (s *Service) handleBatches(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"seed":    s.Suite.Seed,
		"batches": s.BatchIDs(),
		"schemas": []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3},
	})
}

func (s *Service) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	batch := q.Get("batch")
	if batch == "" && len(s.Suite.Order) > 0 {
		batch = s.Suite.Order[0]
	}
	opts := engine.DefaultOptions()
	if q.Get("workers") == "1" {
		opts.Workers = 1
	}
	parallel := q.Get("parallel") == "1" || q.Get("parallel") == "true"
	report, err := s.Verify(r.Context(), batch, q.Get("schema"), parallel, opts)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Service) handleDocument(w http.ResponseWriter, r *http.Request) {
	batch := r.URL.Query().Get("batch")
	version := r.URL.Query().Get("version")
	data, ok := s.Docs[batch][version]
	if !ok {
		writeDomainError(w, domain.NewError(domain.ESchemaUnknown, "document not found").
			With("batch_id", batch).With("schema_version", version))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Service) handleInject(w http.ResponseWriter, r *http.Request) {
	batch := r.URL.Query().Get("batch")
	kind := r.URL.Query().Get("kind")
	project, _, err := s.Project(batch, "canonical")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	_, injectErr := diagnostics.InjectInput(project, kind)
	if injectErr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": "OK", "injection": kind})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        false,
		"injection": kind,
		"code":      domain.CodeOf(injectErr),
		"error":     injectErr.Error(),
	})
}

func (s *Service) handleCrashResume(w http.ResponseWriter, r *http.Request) {
	batch := r.URL.Query().Get("batch")
	if batch == "" && len(s.Suite.Order) > 0 {
		batch = s.Suite.Order[0]
	}
	project, _, err := s.Project(batch, "canonical")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	dbPath := filepath.Join(os.TempDir(), "mvcc-workbench-"+batch+".db")
	report, err := CrashAndResume(context.Background(), project, dbPath, 7, engine.DefaultOptions())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
