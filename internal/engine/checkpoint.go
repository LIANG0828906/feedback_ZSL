package engine

import (
	"context"

	"atelier/internal/model"
)

type CheckpointSink interface {
	BeginSession(ctx context.Context, scenarioHash string, zoneOrder []string) (string, error)
	SaveZone(ctx context.Context, sessionID string, state model.ZoneState) error
	FinishSession(ctx context.Context, sessionID, status string) error
}
