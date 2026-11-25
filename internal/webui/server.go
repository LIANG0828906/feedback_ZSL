package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"skeletonretarget/internal/demo"
)

type Handler struct {
	data *demo.Data
	html []byte
	json []byte
}

func NewHandler(data *demo.Data) (*Handler, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	page := strings.Replace(pageTemplate, "__REPORT_JSON__", string(raw), 1)
	return &Handler{data: data, html: []byte(page), json: raw}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(h.html)
	case "/api/report":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(h.json)
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	default:
		http.NotFound(w, r)
	}
}

func Serve(ctx context.Context, addr string, handler http.Handler) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("diagnostic server only binds to loopback, got %q", host)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("diagnostic page: http://%s/\n", listener.Addr().String())
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
