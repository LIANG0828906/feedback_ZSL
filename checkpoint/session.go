package checkpoint

import (
	"context"

	"storyworkbench/engine"
)

// RecordSession commits one transaction per validated shot boundary. This is
// the reference path used by the demo and by restart verification.
func RecordSession(ctx context.Context, store *Store, session *engine.Session) (*engine.RunResult, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		_, ok, err := session.NextShot()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		save := session.Snapshot()
		if err := store.CommitBoundary(session.Project(), save); err != nil {
			return nil, err
		}
	}
	return session.Result(), nil
}
