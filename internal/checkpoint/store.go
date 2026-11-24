package checkpoint

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"spatialworkbench/internal/acoustics"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

type Generation struct {
	InputHash  string
	ParamsHash string
	Algorithm  string
	LayoutHash string
}

type Record struct {
	Frame       int
	Seq         int
	Params      model.Params
	Result      acoustics.FrameResult
	CommittedAt time.Time
}

type LoadResult struct {
	Records     []Record
	Invalidated bool
	Reason      string
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, *werr.Error) {
	if path == "" {
		return nil, werr.New(werr.CodeState, "checkpoint path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, werr.New(werr.CodeState, "cannot create checkpoint directory: %v", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, werr.New(werr.CodeState, "cannot open sqlite checkpoint: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, werr.New(werr.CodeState, "cannot ping sqlite checkpoint: %v", err)
	}
	store := &Store{db: db, path: path}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, werr.New(werr.CodeState, "cannot migrate checkpoint schema: %v", err)
	}
	return store, nil
}

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			input_hash TEXT NOT NULL,
			params_hash TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			layout_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS committed_frames (
			frame_index INTEGER PRIMARY KEY,
			seq INTEGER NOT NULL UNIQUE,
			input_hash TEXT NOT NULL,
			params_hash TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			layout_hash TEXT NOT NULL,
			params_json TEXT NOT NULL,
			result_json TEXT NOT NULL,
			render_blob BLOB NOT NULL,
			committed_at INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Commit(ctx context.Context, generation Generation, result acoustics.FrameResult) *werr.Error {
	if err := validateGeneration(generation); err != nil {
		return err
	}
	if result.Frame < 0 {
		return werr.New(werr.CodeState, "cannot commit negative frame %d", result.Frame)
	}
	paramsJSON, err := json.Marshal(result.Params)
	if err != nil {
		return werr.New(werr.CodeState, "cannot encode frame parameters: %v", err)
	}
	resultForJSON := result.Clone()
	resultForJSON.Render = nil
	resultJSON, err := json.Marshal(resultForJSON)
	if err != nil {
		return werr.New(werr.CodeState, "cannot encode frame result: %v", err)
	}
	renderBlob, err := encodeRender(result.Render)
	if err != nil {
		return werr.New(werr.CodeState, "cannot encode rendered samples: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return werr.New(werr.CodeState, "cannot begin checkpoint transaction: %v", err)
	}
	defer tx.Rollback()
	existing, ok, err := readGeneration(ctx, tx)
	if err != nil {
		return werr.New(werr.CodeState, "cannot read checkpoint meta: %v", err)
	}
	if !ok {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta
			(id,input_hash,params_hash,algorithm_version,layout_hash,created_at) VALUES(1,?,?,?,?,?)`,
			generation.InputHash, generation.ParamsHash, generation.Algorithm, generation.LayoutHash, time.Now().UnixNano()); err != nil {
			return werr.New(werr.CodeState, "cannot insert checkpoint meta: %v", err)
		}
	} else if existing != generation {
		e := werr.New(werr.CodeState, "checkpoint generation changed during a live session; reopen to invalidate")
		e.Field = "checkpoint.generation"
		return e
	}

	var count, maxSeq, maxFrame int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(seq),-1), COALESCE(MAX(frame_index),-1) FROM committed_frames`).
		Scan(&count, &maxSeq, &maxFrame); err != nil {
		return werr.New(werr.CodeState, "cannot inspect checkpoint sequence: %v", err)
	}
	if result.Frame != count || result.Frame != maxFrame+1 || maxSeq != count-1 {
		e := werr.New(werr.CodeState, "checkpoint frame order is unstable: attempting frame %d after %d records (max frame %d, max seq %d)", result.Frame, count, maxFrame, maxSeq)
		e.Field = "frame"
		e.Frame = result.Frame
		return e
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO committed_frames
		(frame_index,seq,input_hash,params_hash,algorithm_version,layout_hash,params_json,result_json,render_blob,committed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		result.Frame, count, generation.InputHash, generation.ParamsHash, generation.Algorithm, generation.LayoutHash,
		string(paramsJSON), string(resultJSON), renderBlob, time.Now().UnixNano()); err != nil {
		return werr.New(werr.CodeState, "cannot insert committed frame %d: %v", result.Frame, err)
	}
	if err := tx.Commit(); err != nil {
		return werr.New(werr.CodeState, "cannot commit checkpoint transaction: %v", err)
	}
	return nil
}

func (s *Store) Load(ctx context.Context, generation Generation) (LoadResult, *werr.Error) {
	if err := validateGeneration(generation); err != nil {
		return LoadResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoadResult{}, werr.New(werr.CodeState, "cannot begin checkpoint load: %v", err)
	}
	defer tx.Rollback()
	meta, ok, err := readGeneration(ctx, tx)
	if err != nil {
		return LoadResult{}, werr.New(werr.CodeState, "cannot read checkpoint meta: %v", err)
	}
	if !ok {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM committed_frames`).Scan(&count); err != nil {
			return LoadResult{}, werr.New(werr.CodeState, "cannot count checkpoint rows: %v", err)
		}
		if count == 0 {
			return LoadResult{}, nil
		}
		reason := "checkpoint contains frames but no generation metadata"
		if err := clearTx(ctx, tx); err != nil {
			return LoadResult{}, werr.New(werr.CodeState, "cannot invalidate checkpoint: %v", err)
		}
		if err := tx.Commit(); err != nil {
			return LoadResult{}, werr.New(werr.CodeState, "cannot commit checkpoint invalidation: %v", err)
		}
		return LoadResult{Invalidated: true, Reason: reason}, nil
	}
	if meta != generation {
		reason := generationMismatch(meta, generation)
		if err := clearTx(ctx, tx); err != nil {
			return LoadResult{}, werr.New(werr.CodeState, "cannot invalidate mismatched checkpoint: %v", err)
		}
		if err := tx.Commit(); err != nil {
			return LoadResult{}, werr.New(werr.CodeState, "cannot commit checkpoint invalidation: %v", err)
		}
		return LoadResult{Invalidated: true, Reason: reason}, nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT frame_index,seq,input_hash,params_hash,algorithm_version,layout_hash,params_json,result_json,render_blob,committed_at
		FROM committed_frames ORDER BY seq ASC`)
	if err != nil {
		return LoadResult{}, werr.New(werr.CodeState, "cannot query checkpoint frames: %v", err)
	}
	defer rows.Close()
	records := make([]Record, 0)
	for rows.Next() {
		var record Record
		var inputHash, paramsHash, algorithm, layoutHash, paramsJSON, resultJSON string
		var renderBlob []byte
		var committedAt int64
		if err := rows.Scan(&record.Frame, &record.Seq, &inputHash, &paramsHash, &algorithm, &layoutHash, &paramsJSON, &resultJSON, &renderBlob, &committedAt); err != nil {
			return LoadResult{}, werr.New(werr.CodeState, "cannot scan checkpoint frame: %v", err)
		}
		if inputHash != generation.InputHash || paramsHash != generation.ParamsHash || algorithm != generation.Algorithm || layoutHash != generation.LayoutHash {
			rows.Close()
			reason := fmt.Sprintf("frame %d generation does not match active input/parameters/algorithm", record.Frame)
			if err := clearTx(ctx, tx); err != nil {
				return LoadResult{}, werr.New(werr.CodeState, "cannot invalidate mixed checkpoint: %v", err)
			}
			if err := tx.Commit(); err != nil {
				return LoadResult{}, werr.New(werr.CodeState, "cannot commit mixed checkpoint invalidation: %v", err)
			}
			return LoadResult{Invalidated: true, Reason: reason}, nil
		}
		if err := json.Unmarshal([]byte(paramsJSON), &record.Params); err != nil {
			return LoadResult{}, werr.New(werr.CodeDecode, "frame %d parameters are corrupt: %v", record.Frame, err)
		}
		if err := json.Unmarshal([]byte(resultJSON), &record.Result); err != nil {
			return LoadResult{}, werr.New(werr.CodeDecode, "frame %d result is corrupt: %v", record.Frame, err)
		}
		render, err := decodeRender(renderBlob)
		if err != nil {
			return LoadResult{}, werr.New(werr.CodeDecode, "frame %d render is corrupt: %v", record.Frame, err)
		}
		record.Result.Render = render
		record.Result.Params = record.Params
		record.CommittedAt = time.Unix(0, committedAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return LoadResult{}, werr.New(werr.CodeState, "cannot iterate checkpoint frames: %v", err)
	}
	if err := rows.Close(); err != nil {
		return LoadResult{}, werr.New(werr.CodeState, "cannot close checkpoint rows: %v", err)
	}
	for i, record := range records {
		if record.Frame != i || record.Seq != i {
			reason := fmt.Sprintf("checkpoint sequence is not contiguous at record %d (frame=%d seq=%d)", i, record.Frame, record.Seq)
			if err := clearTx(ctx, tx); err != nil {
				return LoadResult{}, werr.New(werr.CodeState, "cannot invalidate non-contiguous checkpoint: %v", err)
			}
			if err := tx.Commit(); err != nil {
				return LoadResult{}, werr.New(werr.CodeState, "cannot commit non-contiguous checkpoint invalidation: %v", err)
			}
			return LoadResult{Invalidated: true, Reason: reason}, nil
		}
	}
	return LoadResult{Records: records}, nil
}

func (s *Store) Clear(ctx context.Context) *werr.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return werr.New(werr.CodeState, "cannot begin checkpoint clear: %v", err)
	}
	defer tx.Rollback()
	if err := clearTx(ctx, tx); err != nil {
		return werr.New(werr.CodeState, "cannot clear checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return werr.New(werr.CodeState, "cannot commit checkpoint clear: %v", err)
	}
	return nil
}

func clearTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM committed_frames`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_meta`); err != nil {
		return err
	}
	return nil
}

func readGeneration(ctx context.Context, tx *sql.Tx) (Generation, bool, error) {
	var generation Generation
	err := tx.QueryRowContext(ctx, `SELECT input_hash,params_hash,algorithm_version,layout_hash FROM checkpoint_meta WHERE id=1`).
		Scan(&generation.InputHash, &generation.ParamsHash, &generation.Algorithm, &generation.LayoutHash)
	if err == sql.ErrNoRows {
		return Generation{}, false, nil
	}
	if err != nil {
		return Generation{}, false, err
	}
	return generation, true, nil
}

func validateGeneration(generation Generation) *werr.Error {
	if generation.InputHash == "" || generation.ParamsHash == "" || generation.Algorithm == "" || generation.LayoutHash == "" {
		return werr.New(werr.CodeState, "checkpoint generation requires input hash, params hash, algorithm version, and layout hash")
	}
	return nil
}

func generationMismatch(old, current Generation) string {
	switch {
	case old.InputHash != current.InputHash:
		return "input content hash changed; all checkpoint frames invalidated"
	case old.ParamsHash != current.ParamsHash:
		return "parameter version changed; all checkpoint frames invalidated"
	case old.Algorithm != current.Algorithm:
		return "algorithm version changed; all checkpoint frames invalidated"
	default:
		return "layout changed; all checkpoint frames invalidated"
	}
}

func encodeRender(render [][]float64) ([]byte, error) {
	if uint64(len(render)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("too many output channels")
	}
	length := -1
	for _, channel := range render {
		if length < 0 {
			length = len(channel)
		} else if len(channel) != length {
			return nil, fmt.Errorf("ragged render")
		}
	}
	if length < 0 {
		length = 0
	}
	header := 12
	buf := make([]byte, header+len(render)*length*8)
	binary.LittleEndian.PutUint32(buf[0:4], 1)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(render)))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(length))
	offset := header
	for _, channel := range render {
		for _, sample := range channel {
			binary.LittleEndian.PutUint64(buf[offset:offset+8], math.Float64bits(sample))
			offset += 8
		}
	}
	return buf, nil
}

func decodeRender(blob []byte) ([][]float64, error) {
	if len(blob) < 12 {
		return nil, fmt.Errorf("render header is truncated")
	}
	if binary.LittleEndian.Uint32(blob[0:4]) != 1 {
		return nil, fmt.Errorf("unsupported render encoding")
	}
	channels := int(binary.LittleEndian.Uint32(blob[4:8]))
	length := int(binary.LittleEndian.Uint32(blob[8:12]))
	expected := 12 + channels*length*8
	if channels < 0 || length < 0 || expected != len(blob) {
		return nil, fmt.Errorf("render length mismatch: expected %d, got %d", expected, len(blob))
	}
	render := make([][]float64, channels)
	offset := 12
	for c := 0; c < channels; c++ {
		render[c] = make([]float64, length)
		for n := 0; n < length; n++ {
			render[c][n] = math.Float64frombits(binary.LittleEndian.Uint64(blob[offset : offset+8]))
			offset += 8
		}
	}
	return render, nil
}
