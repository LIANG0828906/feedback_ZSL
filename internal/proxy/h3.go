package proxy

import (
	"fmt"
	"net/http"

	"feedback_zsl/internal/entry"
)

type H3Handler struct {
	Next http.Handler
}

func (h *H3Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	info, _ := entry.ConnectionInfoFromContext(r.Context())
	connID := "h3-unknown"
	remote := r.RemoteAddr
	var seq uint64
	if info != nil {
		connID = info.ID
		if info.RemoteAddr != "" {
			remote = info.RemoteAddr
		}
		seq = info.NextStreamID()
	}
	meta := entry.Meta{
		Protocol:     "h3",
		StreamID:     fmt.Sprintf("%s-%d", connID, seq),
		ConnectionID: connID,
		RemoteAddr:   remote,
	}
	h.Next.ServeHTTP(w, r.WithContext(entry.WithMeta(r.Context(), meta)))
}
