//go:build cgo

package store

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
typedef long long sqlite3_int64;
typedef void (*sqlite3_destructor_type)(void*);

static void *vf_lib;
static int (*vf_open_v2_fn)(const char*, sqlite3**, int, const char*);
static int (*vf_close_fn)(sqlite3*);
static int (*vf_exec_fn)(sqlite3*, const char*, int (*)(void*,int,char**,char**), void*, char**);
static int (*vf_busy_timeout_fn)(sqlite3*, int);
static const char *(*vf_errmsg_fn)(sqlite3*);
static int (*vf_prepare_v2_fn)(sqlite3*, const char*, int, sqlite3_stmt**, const char**);
static int (*vf_finalize_fn)(sqlite3_stmt*);
static int (*vf_step_fn)(sqlite3_stmt*);
static int (*vf_bind_text_fn)(sqlite3_stmt*, int, const char*, int, sqlite3_destructor_type);
static int (*vf_bind_int64_fn)(sqlite3_stmt*, int, sqlite3_int64);
static int (*vf_bind_double_fn)(sqlite3_stmt*, int, double);
static int (*vf_bind_blob_fn)(sqlite3_stmt*, int, const void*, int, sqlite3_destructor_type);
static sqlite3_int64 (*vf_column_int64_fn)(sqlite3_stmt*, int);
static double (*vf_column_double_fn)(sqlite3_stmt*, int);
static const unsigned char *(*vf_column_text_fn)(sqlite3_stmt*, int);
static const void *(*vf_column_blob_fn)(sqlite3_stmt*, int);
static int (*vf_column_bytes_fn)(sqlite3_stmt*, int);
static const char *(*vf_libversion_fn)(void);
static void (*vf_free_fn)(void*);

static int vf_load(void) {
	if (vf_lib) return 0;
	vf_lib = dlopen("libsqlite3.so.0", RTLD_NOW | RTLD_LOCAL);
	if (!vf_lib) return -1;
	vf_open_v2_fn = (int (*)(const char*, sqlite3**, int, const char*))dlsym(vf_lib, "sqlite3_open_v2");
	vf_close_fn = (int (*)(sqlite3*))dlsym(vf_lib, "sqlite3_close");
	vf_exec_fn = (int (*)(sqlite3*, const char*, int (*)(void*,int,char**,char**), void*, char**))dlsym(vf_lib, "sqlite3_exec");
	vf_busy_timeout_fn = (int (*)(sqlite3*, int))dlsym(vf_lib, "sqlite3_busy_timeout");
	vf_errmsg_fn = (const char *(*)(sqlite3*))dlsym(vf_lib, "sqlite3_errmsg");
	vf_prepare_v2_fn = (int (*)(sqlite3*, const char*, int, sqlite3_stmt**, const char**))dlsym(vf_lib, "sqlite3_prepare_v2");
	vf_finalize_fn = (int (*)(sqlite3_stmt*))dlsym(vf_lib, "sqlite3_finalize");
	vf_step_fn = (int (*)(sqlite3_stmt*))dlsym(vf_lib, "sqlite3_step");
	vf_bind_text_fn = (int (*)(sqlite3_stmt*, int, const char*, int, sqlite3_destructor_type))dlsym(vf_lib, "sqlite3_bind_text");
	vf_bind_int64_fn = (int (*)(sqlite3_stmt*, int, sqlite3_int64))dlsym(vf_lib, "sqlite3_bind_int64");
	vf_bind_double_fn = (int (*)(sqlite3_stmt*, int, double))dlsym(vf_lib, "sqlite3_bind_double");
	vf_bind_blob_fn = (int (*)(sqlite3_stmt*, int, const void*, int, sqlite3_destructor_type))dlsym(vf_lib, "sqlite3_bind_blob");
	vf_column_int64_fn = (sqlite3_int64 (*)(sqlite3_stmt*, int))dlsym(vf_lib, "sqlite3_column_int64");
	vf_column_double_fn = (double (*)(sqlite3_stmt*, int))dlsym(vf_lib, "sqlite3_column_double");
	vf_column_text_fn = (const unsigned char *(*)(sqlite3_stmt*, int))dlsym(vf_lib, "sqlite3_column_text");
	vf_column_blob_fn = (const void *(*)(sqlite3_stmt*, int))dlsym(vf_lib, "sqlite3_column_blob");
	vf_column_bytes_fn = (int (*)(sqlite3_stmt*, int))dlsym(vf_lib, "sqlite3_column_bytes");
	vf_libversion_fn = (const char *(*)(void))dlsym(vf_lib, "sqlite3_libversion");
	vf_free_fn = (void (*)(void*))dlsym(vf_lib, "sqlite3_free");
	return (vf_open_v2_fn && vf_close_fn && vf_exec_fn && vf_prepare_v2_fn &&
	        vf_finalize_fn && vf_step_fn && vf_bind_text_fn && vf_bind_int64_fn &&
	        vf_bind_double_fn && vf_bind_blob_fn && vf_column_int64_fn &&
	        vf_column_double_fn && vf_column_text_fn && vf_column_blob_fn &&
	        vf_column_bytes_fn && vf_errmsg_fn && vf_free_fn) ? 0 : -2;
}

static int vf_open(const char *path, sqlite3 **db) {
	if (vf_load() != 0) return -1000;
	return vf_open_v2_fn(path, db, 0x00000002 | 0x00000004, 0);
}
static int vf_close(sqlite3 *db) { return vf_close_fn(db); }
static int vf_exec(sqlite3 *db, const char *sql) { return vf_exec_fn(db, sql, 0, 0, 0); }
static void vf_busy(sqlite3 *db, int ms) { vf_busy_timeout_fn(db, ms); }
static const char *vf_errmsg(sqlite3 *db) { return vf_errmsg_fn(db); }
static int vf_prepare(sqlite3 *db, const char *sql, sqlite3_stmt **st) { return vf_prepare_v2_fn(db, sql, -1, st, 0); }
static int vf_finalize(sqlite3_stmt *st) { return vf_finalize_fn(st); }
static int vf_step(sqlite3_stmt *st) { return vf_step_fn(st); }
static int vf_bind_text(sqlite3_stmt *st, int i, const char *s) {
	return vf_bind_text_fn(st, i, s, -1, (sqlite3_destructor_type)(intptr_t)-1);
}
static int vf_bind_i64(sqlite3_stmt *st, int i, sqlite3_int64 v) { return vf_bind_int64_fn(st, i, v); }
static int vf_bind_f64(sqlite3_stmt *st, int i, double v) { return vf_bind_double_fn(st, i, v); }
static int vf_bind_blob(sqlite3_stmt *st, int i, const void *p, int n) {
	return vf_bind_blob_fn(st, i, p, n, (sqlite3_destructor_type)(intptr_t)-1);
}
static sqlite3_int64 vf_col_i64(sqlite3_stmt *st, int i) { return vf_column_int64_fn(st, i); }
static double vf_col_f64(sqlite3_stmt *st, int i) { return vf_column_double_fn(st, i); }
static const char *vf_col_text(sqlite3_stmt *st, int i) { return (const char*)vf_column_text_fn(st, i); }
static const void *vf_col_blob(sqlite3_stmt *st, int i) { return vf_column_blob_fn(st, i); }
static int vf_col_bytes(sqlite3_stmt *st, int i) { return vf_column_bytes_fn(st, i); }
static const char *vf_version(void) { if (vf_load() != 0) return "unavailable"; return vf_libversion_fn(); }
static void vf_free(void *p) { if (vf_free_fn) vf_free_fn(p); }
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const (
	sqliteOK   = 0
	sqliteRow  = 100
	sqliteDone = 101
)

type sqliteStore struct {
	mu   sync.Mutex
	db   *C.sqlite3
	path string
}

func openStore(path string) (Store, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var db *C.sqlite3
	if rc := C.vf_open(cpath, &db); rc != sqliteOK {
		if db != nil {
			_ = C.vf_close(db)
		}
		return openFileStore(path)
	}
	s := &sqliteStore{db: db, path: path}
	C.vf_busy(db, 5000)
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=FULL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA temp_store=MEMORY;",
	}
	for _, p := range pragmas {
		if err := s.exec(p); err != nil {
			s.Close()
			return nil, err
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS chunks (
  chunk_x INTEGER NOT NULL,
  chunk_y INTEGER NOT NULL,
  commit_seq INTEGER NOT NULL UNIQUE,
  ray_digest TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  camera_version TEXT NOT NULL,
  scene_version TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  transmittance REAL NOT NULL,
  scattering REAL NOT NULL,
  dependencies BLOB,
  payload BLOB NOT NULL,
  created_unix_us INTEGER NOT NULL,
  PRIMARY KEY (chunk_x, chunk_y)
);
CREATE TABLE IF NOT EXISTS commits (
  seq INTEGER PRIMARY KEY,
  chunk_x INTEGER NOT NULL,
  chunk_y INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  committed_unix_us INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chunks_order ON chunks(commit_seq);
`
	if err := s.exec(schema); err != nil {
		s.Close()
		return nil, err
	}
	_ = s.exec(fmt.Sprintf("INSERT OR REPLACE INTO meta(key,value) VALUES('sqlite_version','%s');", sqliteVersion()))
	return s, nil
}

func sqliteVersion() string { return C.GoString(C.vf_version()) }

func (s *sqliteStore) Path() string    { return s.path }
func (s *sqliteStore) Backend() string { return "sqlite3/" + sqliteVersion() }

func (s *sqliteStore) errmsg() error {
	msg := C.GoString(C.vf_errmsg(s.db))
	if msg == "" {
		msg = "unknown sqlite error"
	}
	return errors.New(msg)
}

func (s *sqliteStore) exec(sql string) error {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	if rc := C.vf_exec(s.db, csql); rc != sqliteOK {
		return fmt.Errorf("sqlite exec: %w", s.errmsg())
	}
	return nil
}

func (s *sqliteStore) prepare(sql string) (*C.sqlite3_stmt, error) {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	var st *C.sqlite3_stmt
	if rc := C.vf_prepare(s.db, csql, &st); rc != sqliteOK {
		return nil, fmt.Errorf("sqlite prepare: %w", s.errmsg())
	}
	return st, nil
}

func bindText(st *C.sqlite3_stmt, idx int, value string) error {
	cv := C.CString(value)
	defer C.free(unsafe.Pointer(cv))
	if rc := C.vf_bind_text(st, C.int(idx), cv); rc != sqliteOK {
		return fmt.Errorf("sqlite bind text[%d]: rc=%d", idx, int(rc))
	}
	return nil
}

func bindI64(st *C.sqlite3_stmt, idx int, value int64) error {
	if rc := C.vf_bind_i64(st, C.int(idx), C.sqlite3_int64(value)); rc != sqliteOK {
		return fmt.Errorf("sqlite bind int64[%d]: rc=%d", idx, int(rc))
	}
	return nil
}

func bindF64(st *C.sqlite3_stmt, idx int, value float64) error {
	if rc := C.vf_bind_f64(st, C.int(idx), C.double(value)); rc != sqliteOK {
		return fmt.Errorf("sqlite bind double[%d]: rc=%d", idx, int(rc))
	}
	return nil
}

func bindBlob(st *C.sqlite3_stmt, idx int, value []byte) error {
	if len(value) == 0 {
		if rc := C.vf_bind_blob(st, C.int(idx), nil, 0); rc != sqliteOK {
			return fmt.Errorf("sqlite bind empty blob[%d]: rc=%d", idx, int(rc))
		}
		return nil
	}
	p := C.CBytes(value)
	defer C.free(p)
	if rc := C.vf_bind_blob(st, C.int(idx), p, C.int(len(value))); rc != sqliteOK {
		return fmt.Errorf("sqlite bind blob[%d]: rc=%d", idx, int(rc))
	}
	return nil
}

func step(st *C.sqlite3_stmt) (int, error) {
	rc := C.vf_step(st)
	if rc == sqliteRow || rc == sqliteDone {
		return int(rc), nil
	}
	return int(rc), fmt.Errorf("sqlite step: rc=%d", int(rc))
}

func colText(st *C.sqlite3_stmt, idx int) string {
	p := C.vf_col_text(st, C.int(idx))
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

func colBlob(st *C.sqlite3_stmt, idx int) []byte {
	n := int(C.vf_col_bytes(st, C.int(idx)))
	if n <= 0 {
		return nil
	}
	p := C.vf_col_blob(st, C.int(idx))
	if p == nil {
		return nil
	}
	return C.GoBytes(p, C.int(n))
}

func (s *sqliteStore) PutChunk(rec ChunkRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exec("BEGIN IMMEDIATE;"); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.exec("ROLLBACK;")
		}
	}()
	st, err := s.prepare(`
INSERT INTO chunks(chunk_x,chunk_y,commit_seq,ray_digest,cache_key,content_hash,
 recipe_version,algorithm_version,camera_version,scene_version,width,height,
 transmittance,scattering,dependencies,payload,created_unix_us)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chunk_x,chunk_y) DO UPDATE SET
 commit_seq=excluded.commit_seq, ray_digest=excluded.ray_digest,
 cache_key=excluded.cache_key, content_hash=excluded.content_hash,
 recipe_version=excluded.recipe_version, algorithm_version=excluded.algorithm_version,
 camera_version=excluded.camera_version, scene_version=excluded.scene_version,
 width=excluded.width, height=excluded.height, transmittance=excluded.transmittance,
 scattering=excluded.scattering, dependencies=excluded.dependencies,
 payload=excluded.payload, created_unix_us=excluded.created_unix_us;`)
	if err != nil {
		return err
	}
	if err := bindI64(st, 1, int64(rec.ChunkX)); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindI64(st, 2, int64(rec.ChunkY)); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindI64(st, 3, rec.CommitSeq); err != nil {
		C.vf_finalize(st)
		return err
	}
	texts := []string{rec.RayDigest, rec.CacheKey, rec.ContentHash, rec.RecipeVersion,
		rec.AlgorithmVersion, rec.CameraVersion, rec.SceneVersion}
	for i, v := range texts {
		if err := bindText(st, 4+i, v); err != nil {
			C.vf_finalize(st)
			return err
		}
	}
	if err := bindI64(st, 11, int64(rec.Width)); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindI64(st, 12, int64(rec.Height)); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindF64(st, 13, rec.Transmittance); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindF64(st, 14, rec.Scattering); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindBlob(st, 15, rec.Dependencies); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindBlob(st, 16, rec.Payload); err != nil {
		C.vf_finalize(st)
		return err
	}
	if err := bindI64(st, 17, rec.CreatedUnixUS); err != nil {
		C.vf_finalize(st)
		return err
	}
	if _, err := step(st); err != nil {
		C.vf_finalize(st)
		return err
	}
	if rc := C.vf_finalize(st); rc != sqliteOK {
		return fmt.Errorf("sqlite finalize: %w", s.errmsg())
	}
	st2, err := s.prepare(`INSERT INTO commits(seq,chunk_x,chunk_y,cache_key,committed_unix_us)
VALUES(?,?,?,?,?) ON CONFLICT(seq) DO UPDATE SET chunk_x=excluded.chunk_x,
 chunk_y=excluded.chunk_y, cache_key=excluded.cache_key,
 committed_unix_us=excluded.committed_unix_us;`)
	if err != nil {
		return err
	}
	if err := bindI64(st2, 1, rec.CommitSeq); err != nil {
		C.vf_finalize(st2)
		return err
	}
	if err := bindI64(st2, 2, int64(rec.ChunkX)); err != nil {
		C.vf_finalize(st2)
		return err
	}
	if err := bindI64(st2, 3, int64(rec.ChunkY)); err != nil {
		C.vf_finalize(st2)
		return err
	}
	if err := bindText(st2, 4, rec.CacheKey); err != nil {
		C.vf_finalize(st2)
		return err
	}
	if err := bindI64(st2, 5, rec.CreatedUnixUS); err != nil {
		C.vf_finalize(st2)
		return err
	}
	if _, err := step(st2); err != nil {
		C.vf_finalize(st2)
		return err
	}
	if rc := C.vf_finalize(st2); rc != sqliteOK {
		return fmt.Errorf("sqlite finalize: %w", s.errmsg())
	}
	if err := s.exec("COMMIT;"); err != nil {
		return err
	}
	ok = true
	return nil
}

const chunkSelect = `SELECT chunk_x,chunk_y,commit_seq,ray_digest,cache_key,content_hash,
 recipe_version,algorithm_version,camera_version,scene_version,width,height,
 transmittance,scattering,dependencies,payload,created_unix_us FROM chunks`

func scanChunk(st *C.sqlite3_stmt) ChunkRecord {
	return ChunkRecord{
		ChunkX:           int(C.vf_col_i64(st, 0)),
		ChunkY:           int(C.vf_col_i64(st, 1)),
		CommitSeq:        int64(C.vf_col_i64(st, 2)),
		RayDigest:        colText(st, 3),
		CacheKey:         colText(st, 4),
		ContentHash:      colText(st, 5),
		RecipeVersion:    colText(st, 6),
		AlgorithmVersion: colText(st, 7),
		CameraVersion:    colText(st, 8),
		SceneVersion:     colText(st, 9),
		Width:            int(C.vf_col_i64(st, 10)),
		Height:           int(C.vf_col_i64(st, 11)),
		Transmittance:    float64(C.vf_col_f64(st, 12)),
		Scattering:       float64(C.vf_col_f64(st, 13)),
		Dependencies:     colBlob(st, 14),
		Payload:          colBlob(st, 15),
		CreatedUnixUS:    int64(C.vf_col_i64(st, 16)),
	}
}

func (s *sqliteStore) GetChunk(x, y int) (ChunkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.prepare(chunkSelect + " WHERE chunk_x=? AND chunk_y=?;")
	if err != nil {
		return ChunkRecord{}, err
	}
	defer C.vf_finalize(st)
	_ = bindI64(st, 1, int64(x))
	_ = bindI64(st, 2, int64(y))
	rc, err := step(st)
	if err != nil {
		return ChunkRecord{}, err
	}
	if rc == sqliteDone {
		return ChunkRecord{}, ErrNotFound
	}
	return scanChunk(st), nil
}

func (s *sqliteStore) ListChunks() ([]ChunkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.prepare(chunkSelect + " ORDER BY commit_seq,chunk_y,chunk_x;")
	if err != nil {
		return nil, err
	}
	defer C.vf_finalize(st)
	var out []ChunkRecord
	for {
		rc, err := step(st)
		if err != nil {
			return nil, err
		}
		if rc == sqliteDone {
			break
		}
		out = append(out, scanChunk(st))
	}
	return out, nil
}

func (s *sqliteStore) DeleteChunk(x, y int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exec("BEGIN IMMEDIATE;"); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.exec("ROLLBACK;")
		}
	}()
	st, err := s.prepare("DELETE FROM chunks WHERE chunk_x=? AND chunk_y=?;")
	if err != nil {
		return err
	}
	_ = bindI64(st, 1, int64(x))
	_ = bindI64(st, 2, int64(y))
	_, err = step(st)
	C.vf_finalize(st)
	if err != nil {
		return err
	}
	st2, err := s.prepare("DELETE FROM commits WHERE chunk_x=? AND chunk_y=?;")
	if err != nil {
		return err
	}
	_ = bindI64(st2, 1, int64(x))
	_ = bindI64(st2, 2, int64(y))
	_, err = step(st2)
	C.vf_finalize(st2)
	if err != nil {
		return err
	}
	if err := s.exec("COMMIT;"); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *sqliteStore) TruncateFrom(seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exec("BEGIN IMMEDIATE;"); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.exec("ROLLBACK;")
		}
	}()
	for _, q := range []string{
		"DELETE FROM chunks WHERE commit_seq>=?;",
		"DELETE FROM commits WHERE seq>=?;",
	} {
		st, err := s.prepare(q)
		if err != nil {
			return err
		}
		_ = bindI64(st, 1, seq)
		_, err = step(st)
		C.vf_finalize(st)
		if err != nil {
			return err
		}
	}
	if err := s.exec("COMMIT;"); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *sqliteStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exec("BEGIN IMMEDIATE;"); err != nil {
		return err
	}
	if err := s.exec("DELETE FROM chunks; DELETE FROM commits;"); err != nil {
		_ = s.exec("ROLLBACK;")
		return err
	}
	if err := s.exec("COMMIT;"); err != nil {
		return err
	}
	return nil
}

func (s *sqliteStore) Commits() ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.prepare("SELECT seq,chunk_x,chunk_y,cache_key,committed_unix_us FROM commits ORDER BY seq;")
	if err != nil {
		return nil, err
	}
	defer C.vf_finalize(st)
	var out []CommitRecord
	for {
		rc, err := step(st)
		if err != nil {
			return nil, err
		}
		if rc == sqliteDone {
			break
		}
		out = append(out, CommitRecord{
			Seq:             int64(C.vf_col_i64(st, 0)),
			ChunkX:          int(C.vf_col_i64(st, 1)),
			ChunkY:          int(C.vf_col_i64(st, 2)),
			CacheKey:        colText(st, 3),
			CommittedUnixUS: int64(C.vf_col_i64(st, 4)),
		})
	}
	return out, nil
}

func (s *sqliteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	rc := C.vf_close(s.db)
	s.db = nil
	if rc != sqliteOK {
		return fmt.Errorf("sqlite close: rc=%d", int(rc))
	}
	return nil
}
