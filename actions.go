package iconcut

import "context"

func RetryTile(ctx context.Context, dbPath, imageID string, tileIndex int, stage string) error {
	st, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Retry(ctx, imageID, TileKey{TileIndex: tileIndex, Stage: stage})
}
