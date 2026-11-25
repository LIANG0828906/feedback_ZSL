package diagnostics

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"

	"lipsync/domain"
	"lipsync/truth"
)

type State struct {
	ProjectID        string                    `json:"project_id"`
	SourceVersion    string                    `json:"source_version"`
	TruthFingerprint string                    `json:"truth_fingerprint"`
	Render           domain.RenderConfig       `json:"render"`
	PackageCount     int                       `json:"package_count"`
	FrameCount       int                       `json:"frame_count"`
	Verification     any                       `json:"verification,omitempty"`
	Packages         []domain.ScheduledPackage `json:"-"`
	Frames           []domain.FrameRecord      `json:"-"`
	Truth            *truth.Manifest           `json:"-"`
}

func Handler(state *State) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("/v1/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		dialogue := r.URL.Query().Get("dialogue")
		var out []domain.ScheduledPackage
		for _, p := range state.Packages {
			if dialogue == "" || p.DialogueID == dialogue {
				out = append(out, p)
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/v1/frames", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if _, ok := r.URL.Query()["to"]; !ok {
			to = 1<<62 - 1
		}
		var out []domain.FrameRecord
		for _, f := range state.Frames {
			if f.Frame >= from && f.Frame <= to {
				out = append(out, f)
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/v1/truth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, state.Truth)
	})
	return mux
}

// ListenFirstFree binds the first available port in [start,end].
func ListenFirstFree(start, end int) (net.Listener, int, error) {
	if start < 1 || end > 65535 || start > end {
		return nil, 0, fmt.Errorf("invalid port range %d..%d", start, end)
	}
	for port := start; port <= end; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no free local port in range %d..%d", start, end)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func SortedPackages(pkgs []domain.ScheduledPackage) []domain.ScheduledPackage {
	out := append([]domain.ScheduledPackage(nil), pkgs...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
