package sim

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"dnssim/internal/dnswire"
)

type API struct {
	service  *Service
	shutdown func()
}

func NewAPI(service *Service, shutdown func()) *API {
	return &API{service: service, shutdown: shutdown}
}

func (a *API) Handler() http.Handler {
	return http.HandlerFunc(a.route)
}

func (a *API) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz" || r.URL.Path == "/api/v1/healthz":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, "GET")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case r.URL.Path == "/api/v1/stats":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, "GET")
			return
		}
		writeJSON(w, http.StatusOK, a.service.Stats())
	case r.URL.Path == "/api/v1/traces" || r.URL.Path == "/trace":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "POST")
			return
		}
		a.trace(w, r)
	case r.URL.Path == "/api/v1/sites":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, "GET")
			return
		}
		a.sites(w)
	case strings.HasPrefix(r.URL.Path, "/api/v1/sites/"):
		a.siteRoute(w, r)
	case r.URL.Path == "/api/v1/admin/shutdown":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "POST")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "shutting_down"})
		if a.shutdown != nil {
			go a.shutdown()
		}
	default:
		writeError(w, http.StatusNotFound, &BusinessError{Code: "DNS_NOT_FOUND", Message: "route not found"})
	}
}

func (a *API) trace(w http.ResponseWriter, r *http.Request) {
	var request TraceRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		return
	}
	result, err := a.service.Trace(r.Context(), request)
	if err != nil {
		writeBusinessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) sites(w http.ResponseWriter) {
	type siteView struct {
		ID        string `json:"id"`
		Recursive string `json:"recursive"`
	}
	sites := make([]siteView, 0, len(a.service.engine.cfg.Sites))
	for _, site := range a.service.engine.cfg.Sites {
		sites = append(sites, siteView{ID: site.ID, Recursive: site.Recursive})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

func (a *API) siteRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sites/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "static-mappings" {
		writeError(w, http.StatusNotFound, &BusinessError{Code: "DNS_NOT_FOUND", Message: "route not found"})
		return
	}
	siteID, err := url.PathUnescape(parts[0])
	if err != nil || siteID == "" {
		writeError(w, http.StatusBadRequest, &BusinessError{Code: CodeInvalidRequest, Message: "invalid site id"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		mappings, err := a.service.StaticMappings(siteID)
		if err != nil {
			writeBusinessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"site": siteID, "static_mappings": mappings})
	case http.MethodPost, http.MethodPut:
		var mapping StaticMapping
		if err := decodeJSONBody(w, r, &mapping); err != nil {
			return
		}
		if err := a.service.SetStaticMapping(siteID, mapping.Name, mapping.Value); err != nil {
			writeBusinessError(w, err)
			return
		}
		canonical, _ := dnswire.CanonicalName(mapping.Name)
		writeJSON(w, http.StatusOK, map[string]any{
			"site": siteID, "name": canonical, "value": mapping.Value, "changed": true,
		})
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" || r.URL.Query().Get("all") == "true" {
			removed, err := a.service.ClearStaticMappings(siteID)
			if err != nil {
				writeBusinessError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"site": siteID, "removed": removed})
			return
		}
		removed, err := a.service.DeleteStaticMapping(siteID, name)
		if err != nil {
			writeBusinessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"site": siteID, "removed": removed})
	default:
		writeMethodNotAllowed(w, "GET, POST, PUT, DELETE")
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, &BusinessError{Code: CodeRequestBodyTooLarge, Message: "request body exceeds 1 MiB"})
			return err
		}
		writeError(w, http.StatusBadRequest, &BusinessError{Code: CodeMalformedJSON, Message: "request body must be a JSON object"})
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, &BusinessError{Code: CodeMalformedJSON, Message: "request body must contain one JSON object"})
		return err
	}
	return nil
}

func writeBusinessError(w http.ResponseWriter, err error) {
	be := asBusinessError(err)
	status := http.StatusInternalServerError
	switch be.Code {
	case CodeInvalidRequest, CodeInvalidType, CodeMalformedJSON:
		status = http.StatusBadRequest
	case CodeUnknownSite:
		status = http.StatusNotFound
	case CodeBusy, CodeQueueTimeout:
		status = http.StatusTooManyRequests
	case CodeShuttingDown:
		status = http.StatusServiceUnavailable
	case CodeRequestBodyTooLarge:
		status = http.StatusRequestEntityTooLarge
	}
	writeError(w, status, be)
}

func writeError(w http.ResponseWriter, status int, err *BusinessError) {
	writeJSON(w, status, map[string]any{"error": err})
}

func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, &BusinessError{Code: CodeMethodNotAllowed, Message: "method not allowed"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
