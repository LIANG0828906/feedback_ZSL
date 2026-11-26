//go:build cgo && linux

package checkpoint

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

typedef struct sqlite3 sqlite3;
typedef int (*icon_open_fn)(const char*, sqlite3**, int, const char*);
typedef int (*icon_close_fn)(sqlite3*);
typedef int (*icon_exec_fn)(sqlite3*, const char*, void*, void*, char**);
typedef char* (*icon_errmsg_fn)(sqlite3*);
typedef void (*icon_free_fn)(void*);

typedef struct icon_db {
	void* lib;
	sqlite3* db;
	icon_open_fn open;
	icon_close_fn close_db;
	icon_exec_fn exec;
	icon_errmsg_fn errmsg;
	icon_free_fn free_mem;
} icon_db;

int icon_sqlite_open(const char* path, icon_db** out, char** dl_error) {
	*out = 0;
	*dl_error = 0;
	void* lib = dlopen("libsqlite3.so.0", RTLD_NOW | RTLD_LOCAL);
	if (!lib) lib = dlopen("libsqlite3.so", RTLD_NOW | RTLD_LOCAL);
	if (!lib) {
		*dl_error = (char*)dlerror();
		return -1;
	}
	icon_db* d = (icon_db*)calloc(1, sizeof(icon_db));
	if (!d) { dlclose(lib); return -2; }
	d->lib = lib;
	*(void**)(&d->open) = dlsym(lib, "sqlite3_open_v2");
	*(void**)(&d->close_db) = dlsym(lib, "sqlite3_close_v2");
	*(void**)(&d->exec) = dlsym(lib, "sqlite3_exec");
	*(void**)(&d->errmsg) = dlsym(lib, "sqlite3_errmsg");
	*(void**)(&d->free_mem) = dlsym(lib, "sqlite3_free");
	if (!d->open || !d->close_db || !d->exec || !d->errmsg || !d->free_mem) {
		free(d); dlclose(lib);
		*dl_error = "missing sqlite3 symbols";
		return -3;
	}
	int rc = d->open(path, &d->db, 6, 0);
	if (rc != 0) {
		if (d->db) d->close_db(d->db);
		free(d); dlclose(lib);
		return rc;
	}
	*out = d;
	return 0;
}

void icon_sqlite_close(icon_db* d) {
	if (!d) return;
	if (d->db && d->close_db) d->close_db(d->db);
	if (d->lib) dlclose(d->lib);
	free(d);
}

int icon_sqlite_exec(icon_db* d, const char* sql, char** sqlite_error) {
	*sqlite_error = 0;
	return d->exec(d->db, sql, 0, 0, sqlite_error);
}

void icon_sqlite_free_error(icon_db* d, char* error_text) {
	if (error_text && d && d->free_mem) d->free_mem(error_text);
}

typedef struct icon_buffer {
	char* bytes;
	size_t length;
} icon_buffer;

static int icon_query_append(void* context, int argc, char** argv, char** columns) {
	(void)columns;
	icon_buffer* buffer = (icon_buffer*)context;
	for (int i = 0; i < argc; i++) {
		const char* value = argv[i] ? argv[i] : "";
		size_t length = strlen(value);
		char* next = (char*)realloc(buffer->bytes, buffer->length + length + 2);
		if (!next) return 1;
		buffer->bytes = next;
		memcpy(buffer->bytes + buffer->length, value, length);
		buffer->length += length;
		buffer->bytes[buffer->length++] = '\n';
		buffer->bytes[buffer->length] = 0;
	}
	return 0;
}

int icon_sqlite_query(icon_db* d, const char* sql, char** output, char** sqlite_error) {
	*output = 0;
	*sqlite_error = 0;
	icon_buffer buffer = {0, 0};
	int rc = d->exec(d->db, sql, icon_query_append, &buffer, sqlite_error);
	if (rc != 0) {
		free(buffer.bytes);
		return rc;
	}
	if (!buffer.bytes) {
		buffer.bytes = (char*)calloc(1, 1);
	} else if (buffer.length > 0) {
		buffer.bytes[buffer.length] = 0;
	}
	*output = buffer.bytes;
	return 0;
}

void icon_sqlite_free_text(char* text) { free(text); }
*/
import "C"

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"iconflow/errs"
)

type SQLiteStore struct {
	path      string
	handle    *C.icon_db
	mu        sync.Mutex
	stageSeq  int64
	windowSeq int64
}

const sqliteSchema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS stage_commits (
  seq INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  block_key TEXT NOT NULL,
  stage TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  threshold INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  artifact_json TEXT,
  committed_at TEXT NOT NULL,
  payload TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS window_commits (
  seq INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  window_id TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  payload TEXT NOT NULL
);
`

func OpenSQLite(path string) (*SQLiteStore, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	var handle *C.icon_db
	var openError *C.char
	rc := C.icon_sqlite_open(cPath, &handle, &openError)
	if rc != 0 {
		message := "sqlite unavailable"
		if openError != nil {
			message = C.GoString(openError)
		}
		return nil, errs.New(errs.ECheck, path, "open sqlite rc=%d: %s", int(rc), message)
	}
	store := &SQLiteStore{path: path, handle: handle}
	if err := store.execLocked(sqliteSchema); err != nil {
		C.icon_sqlite_close(handle)
		return nil, err
	}
	seq, err := store.maxSeqLocked("stage_commits")
	if err != nil {
		C.icon_sqlite_close(handle)
		return nil, err
	}
	store.stageSeq = seq
	seq, err = store.maxSeqLocked("window_commits")
	if err != nil {
		C.icon_sqlite_close(handle)
		return nil, err
	}
	store.windowSeq = seq
	return store, nil
}

func (s *SQLiteStore) execLocked(sql string) error {
	cSQL := C.CString(sql)
	defer C.free(unsafe.Pointer(cSQL))
	var sqliteError *C.char
	rc := C.icon_sqlite_exec(s.handle, cSQL, &sqliteError)
	if rc != 0 {
		message := "unknown sqlite error"
		if sqliteError != nil {
			message = C.GoString(sqliteError)
			C.icon_sqlite_free_error(s.handle, sqliteError)
		}
		return errs.New(errs.ECheck, s.path, "sqlite exec rc=%d: %s", int(rc), message)
	}
	return nil
}

func (s *SQLiteStore) queryTextLocked(sql string) (string, error) {
	cSQL := C.CString(sql)
	defer C.free(unsafe.Pointer(cSQL))
	var output *C.char
	var sqliteError *C.char
	rc := C.icon_sqlite_query(s.handle, cSQL, &output, &sqliteError)
	if rc != 0 {
		message := "unknown sqlite error"
		if sqliteError != nil {
			message = C.GoString(sqliteError)
			C.icon_sqlite_free_error(s.handle, sqliteError)
		}
		return "", errs.New(errs.ECheck, s.path, "sqlite query rc=%d: %s", int(rc), message)
	}
	defer C.icon_sqlite_free_text(output)
	if output == nil {
		return "", nil
	}
	return C.GoString(output), nil
}

func (s *SQLiteStore) maxSeqLocked(table string) (int64, error) {
	text, err := s.queryTextLocked("SELECT COALESCE(MAX(seq),0) FROM " + table + ";")
	if err != nil {
		return 0, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	firstLine := strings.Split(text, "\n")[0]
	return strconv.ParseInt(strings.TrimSpace(firstLine), 10, 64)
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (s *SQLiteStore) CommitStage(_ context.Context, commit StageCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stageSeq++
	commit.Seq = s.stageSeq
	if commit.CommittedAt.IsZero() {
		commit.CommittedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(commit)
	if err != nil {
		s.stageSeq--
		return 0, errs.New(errs.ECheck, "$.stage_payload", "%v", err)
	}
	sql := "BEGIN IMMEDIATE; INSERT INTO stage_commits VALUES (" +
		strconv.FormatInt(commit.Seq, 10) + "," +
		sqlQuote(commit.ProjectID) + "," + sqlQuote(commit.BlockKey) + "," +
		sqlQuote(commit.Stage) + "," + sqlQuote(commit.Status) + "," +
		sqlQuote(commit.Summary) + "," + strconv.Itoa(commit.Threshold) + "," +
		sqlQuote(commit.CacheKey) + "," + sqlQuote(commit.InputHash) + "," +
		sqlQuote(commit.ContentHash) + "," + sqlQuote(commit.ParamVersion) + "," +
		sqlQuote(commit.AlgoVersion) + "," + sqlQuote(string(commit.ArtifactJSON)) + "," +
		sqlQuote(commit.CommittedAt.Format(time.RFC3339Nano)) + "," +
		sqlQuote(string(payload)) + "); COMMIT;"
	if err := s.execLocked(sql); err != nil {
		_ = s.execLocked("ROLLBACK;")
		s.stageSeq--
		return 0, err
	}
	return commit.Seq, nil
}

func (s *SQLiteStore) CommitWindow(_ context.Context, commit WindowCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windowSeq++
	commit.Seq = s.windowSeq
	if commit.CommittedAt.IsZero() {
		commit.CommittedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(commit)
	if err != nil {
		s.windowSeq--
		return 0, errs.New(errs.ECheck, "$.window_payload", "%v", err)
	}
	sql := "BEGIN IMMEDIATE; INSERT INTO window_commits VALUES (" +
		strconv.FormatInt(commit.Seq, 10) + "," +
		sqlQuote(commit.ProjectID) + "," + sqlQuote(commit.WindowID) + "," +
		sqlQuote(commit.Status) + "," + sqlQuote(commit.Summary) + "," +
		sqlQuote(commit.ContentHash) + "," + sqlQuote(commit.ParamVersion) + "," +
		sqlQuote(commit.AlgoVersion) + "," +
		sqlQuote(commit.CommittedAt.Format(time.RFC3339Nano)) + "," +
		sqlQuote(string(payload)) + "); COMMIT;"
	if err := s.execLocked(sql); err != nil {
		_ = s.execLocked("ROLLBACK;")
		s.windowSeq--
		return 0, err
	}
	return commit.Seq, nil
}

func decodePayloadLines[T any](text string) ([]T, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return []T{}, nil
	}
	lines := strings.Split(text, "\n")
	out := make([]T, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var value T
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return nil, errs.New(errs.ECheck, "$.payload", "decode sqlite payload: %v", err)
		}
		out = append(out, value)
	}
	return out, nil
}

func (s *SQLiteStore) StageHistory(ctx context.Context, projectID string) ([]StageCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, err := s.queryTextLocked("SELECT payload FROM stage_commits WHERE project_id=" + sqlQuote(projectID) + " ORDER BY seq;")
	if err != nil {
		return nil, err
	}
	return decodePayloadLines[StageCommit](text)
}

func (s *SQLiteStore) WindowHistory(ctx context.Context, projectID string) ([]WindowCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, err := s.queryTextLocked("SELECT payload FROM window_commits WHERE project_id=" + sqlQuote(projectID) + " ORDER BY seq;")
	if err != nil {
		return nil, err
	}
	return decodePayloadLines[WindowCommit](text)
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != nil {
		C.icon_sqlite_close(s.handle)
		s.handle = nil
	}
	return nil
}
