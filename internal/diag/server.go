package diag

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"haptickernel/internal/device"
)

// HTTPServer 是诊断界面 HTTP 服务，只绑定回环地址。
type HTTPServer struct {
	state *StateHolder
	srv   *http.Server
	Addr  string
}

func NewHTTPServer(state *StateHolder) *HTTPServer {
	return &HTTPServer{state: state}
}

func (h *HTTPServer) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/api/state", h.handleState)
	mux.HandleFunc("/api/retry", h.handleRetry)
	h.srv = &http.Server{Handler: mux}
	h.Addr = "http://" + ln.Addr().String()
	go func() { _ = h.srv.Serve(ln) }()
	return h.Addr, nil
}

func (h *HTTPServer) Close() error { return h.srv.Close() }

func (h *HTTPServer) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.state.Snapshot())
}

// handleRetry 重新跑一次设备回环，可通过表单选择注入的故障。
func (h *HTTPServer) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	}
	fault := device.FaultSpec{
		MissSeqs:    parseSeqs(r.FormValue("miss")),
		DupSeqs:     parseSeqs(r.FormValue("dup")),
		TimeoutSeqs: parseSeqs(r.FormValue("timeout")),
	}
	if err := h.state.RunDeviceLoop(fault); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func parseSeqs(s string) []int {
	out := []int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(part, "%d", &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func (h *HTTPServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	st := h.state.Snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(renderPage(&st)))
}
