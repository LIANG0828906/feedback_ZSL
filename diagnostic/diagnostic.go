// Package diagnostic is the public integration entry point for the local
// prosody diagnostic application.
package diagnostic

import (
	"net/http"

	internaldiag "prosodyalign/internal/diag"
	"prosodyalign/internal/pipeline"
)

type Server = internaldiag.Server
type VerificationReport = pipeline.VerificationReport

func NewServer(workDir string) (*Server, error) {
	return internaldiag.NewServer(workDir)
}

func Handler(server *Server) http.Handler {
	return server.Handler()
}
