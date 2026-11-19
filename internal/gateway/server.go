package gateway

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"s3gw/internal/backend"
	"s3gw/internal/config"
	"s3gw/internal/monitor"
	"s3gw/internal/s3err"
	"s3gw/internal/sig"
	"s3gw/internal/store"
)

type Server struct {
	cfg          config.Config
	store        *store.Store
	backend      *backend.Client
	verifier     *sig.Verifier
	metrics      *monitor.Metrics
	locks        *keyedLocks
	backendReady sync.Map
	logger       *log.Logger
}

func New(cfg config.Config, st *store.Store, be *backend.Client, metrics *monitor.Metrics, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		cfg:     cfg,
		store:   st,
		backend: be,
		metrics: metrics,
		locks:   &keyedLocks{},
		logger:  logger,
		verifier: &sig.Verifier{
			AccessKey: cfg.AccessKey,
			SecretKey: cfg.SecretKey,
			Region:    cfg.Region,
			ClockSkew: cfg.ClockSkew,
		},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := s3err.RequestID()
	hostID := s3err.RequestID()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", hostID)
	w.Header().Set("Server", "s3gw")
	resource := r.URL.EscapedPath()
	if resource == "" {
		resource = "/"
	}
	bucket, _ := splitPath(r.URL.Path)
	versionLabel, _ := detectVersion(r)

	if s.handleInternal(w, r, requestID, hostID) {
		s.logRequest(r, started, requestID)
		return
	}

	authResult, authErr := s.verifier.Authenticate(r)
	if authErr != nil {
		if versionLabel == "" {
			versionLabel = monitor.None
		}
		s.metrics.IncRequest(versionLabel, bucket, operationName(r))
		s.metrics.IncFailure(versionLabel, bucket, authErr.Code)
		s3err.Write(w, requestID, hostID, resource, authErr)
		s.logRequest(r, started, requestID)
		return
	}
	op := operationName(r)
	s.metrics.IncRequest(monitor.VersionLabel(authResult.Version), bucket, op)
	body, readErr := s.readRequestBody(r)
	if readErr != nil {
		s.metrics.IncFailure(monitor.VersionLabel(authResult.Version), bucket, readErr.Code)
		s3err.Write(w, requestID, hostID, resource, readErr)
		s.logRequest(r, started, requestID)
		return
	}
	decodedBody, payloadErr := s.verifier.VerifyPayload(authResult, r, body)
	if payloadErr != nil {
		s.metrics.IncFailure(monitor.VersionLabel(authResult.Version), bucket, payloadErr.Code)
		s3err.Write(w, requestID, hostID, resource, payloadErr)
		s.logRequest(r, started, requestID)
		return
	}
	body = decodedBody

	if err := s.route(w, r, body, bucket, requestID, hostID); err != nil {
		s3err.Write(w, requestID, hostID, resource, err)
	}
	s.logRequest(r, started, requestID)
}

func (s *Server) logRequest(r *http.Request, started time.Time, requestID string) {
	if s.cfg.LogRequests {
		s.logger.Printf("%s %s %s rid=%s elapsed=%s", r.RemoteAddr, r.Method, r.URL.Path, requestID, time.Since(started).Round(time.Millisecond))
	}
}

func (s *Server) readRequestBody(r *http.Request) ([]byte, *s3err.Error) {
	if r.Body == nil {
		return nil, nil
	}
	if r.ContentLength > s.cfg.MaxObjectBytes {
		return nil, s3err.EntityTooLarge()
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxObjectBytes+1))
	if err != nil {
		return nil, s3err.Wrap("IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.", http.StatusBadRequest, err)
	}
	if int64(len(body)) > s.cfg.MaxObjectBytes {
		return nil, s3err.EntityTooLarge()
	}
	if r.ContentLength >= 0 && int64(len(body)) != r.ContentLength {
		return nil, s3err.New("IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.", http.StatusBadRequest)
	}
	return body, nil
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, body []byte, bucket, requestID, hostID string) *s3err.Error {
	method := r.Method
	path := r.URL.Path
	if path == "/" && bucket == "" {
		if method == http.MethodGet {
			return s.listBuckets(w, r)
		}
		return s3err.MethodNotAllowed()
	}
	_, key := splitPath(path)
	q := r.URL.Query()

	if key == "" {
		switch method {
		case http.MethodPut:
			if q.Has("versioning") {
				return s.putBucketVersioning(w, r, body, bucket)
			}
			return s.createBucket(w, r, bucket)
		case http.MethodHead:
			return s.headBucket(w, r, bucket)
		case http.MethodDelete:
			return s.deleteBucket(w, r, bucket)
		case http.MethodGet:
			if q.Has("versioning") {
				return s.getBucketVersioning(w, r, bucket)
			}
			if q.Has("location") {
				return s.getBucketLocation(w, r, bucket)
			}
			if q.Has("versions") {
				return s.listVersions(w, r, bucket)
			}
			if q.Has("uploads") {
				return s.listMultipartUploads(w, r, bucket)
			}
			return s.listObjectsV2(w, r, bucket)
		default:
			return s3err.MethodNotAllowed()
		}
	}

	switch method {
	case http.MethodPut:
		if q.Has("uploadId") {
			return s.uploadPart(w, r, body, bucket, key)
		}
		return s.putObject(w, r, body, bucket, key)
	case http.MethodGet:
		if q.Has("uploadId") {
			return s.listParts(w, r, bucket, key)
		}
		return s.getObject(w, r, bucket, key, false)
	case http.MethodHead:
		return s.getObject(w, r, bucket, key, true)
	case http.MethodDelete:
		if q.Has("uploadId") {
			return s.abortMultipart(w, r, bucket, key)
		}
		return s.deleteObject(w, r, bucket, key)
	case http.MethodPost:
		if q.Has("uploads") {
			return s.initiateMultipart(w, r, bucket, key)
		}
		if q.Has("uploadId") {
			return s.completeMultipart(w, r, body, bucket, key)
		}
		return s3err.MethodNotAllowed()
	default:
		return s3err.MethodNotAllowed()
	}
}

func (s *Server) handleInternal(w http.ResponseWriter, r *http.Request, requestID, hostID string) bool {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		return true
	case "/metrics":
		text, err := s.metrics.Render(r.Context(), s.store)
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusInternalServerError)
			return true
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, text)
		return true
	case "/_gateway/status":
		s.handleStatus(w, r)
		return true
	case "/_gateway/reap":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		res := s.ReapOnce(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
		return true
	default:
		return false
	}
}

func splitPath(path string) (bucket, key string) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", ""
	}
	bucket, key, _ = strings.Cut(path, "/")
	return bucket, key
}

func detectVersion(r *http.Request) (monitor.VersionLabel, bool) {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != "" {
		return monitor.V4, true
	}
	if q.Get("AWSAccessKeyId") != "" || q.Get("Signature") != "" {
		return monitor.V2, true
	}
	auth := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(auth, "AWS4-HMAC-SHA256"):
		return monitor.V4, true
	case strings.HasPrefix(auth, "AWS "):
		return monitor.V2, true
	default:
		return monitor.None, false
	}
}

func operationName(r *http.Request) string {
	_, key := splitPath(r.URL.Path)
	q := r.URL.Query()
	if key == "" {
		switch {
		case r.Method == http.MethodPut && q.Has("versioning"):
			return "PutBucketVersioning"
		case r.Method == http.MethodGet && q.Has("versioning"):
			return "GetBucketVersioning"
		case r.Method == http.MethodGet && q.Has("location"):
			return "GetBucketLocation"
		case r.Method == http.MethodGet && q.Has("versions"):
			return "ListObjectVersions"
		case r.Method == http.MethodGet && q.Has("uploads"):
			return "ListMultipartUploads"
		case r.Method == http.MethodGet && r.URL.Path == "/":
			return "ListBuckets"
		case r.Method == http.MethodGet:
			return "ListObjectsV2"
		case r.Method == http.MethodPut:
			return "CreateBucket"
		case r.Method == http.MethodDelete:
			return "DeleteBucket"
		case r.Method == http.MethodHead:
			return "HeadBucket"
		}
	}
	switch {
	case q.Has("uploads"):
		return "CreateMultipartUpload"
	case q.Has("uploadId") && r.Method == http.MethodPut:
		return "UploadPart"
	case q.Has("uploadId") && r.Method == http.MethodPost:
		return "CompleteMultipartUpload"
	case q.Has("uploadId") && r.Method == http.MethodDelete:
		return "AbortMultipartUpload"
	case q.Has("uploadId") && r.Method == http.MethodGet:
		return "ListParts"
	case r.Method == http.MethodPut:
		return "PutObject"
	case r.Method == http.MethodGet:
		return "GetObject"
	case r.Method == http.MethodHead:
		return "HeadObject"
	case r.Method == http.MethodDelete:
		return "DeleteObject"
	default:
		return "Unknown"
	}
}

func (s *Server) bucketExists(ctx context.Context, bucket string) (bool, *s3err.Error) {
	ok, err := s.store.BucketExists(ctx, bucket)
	if err != nil {
		return false, s3err.Internal(err)
	}
	if !ok {
		return false, s3err.NoSuchBucket()
	}
	return true, nil
}

func (s *Server) ensureBackendBucket(ctx context.Context, bucket string) error {
	if _, ok := s.backendReady.Load(bucket); ok {
		return nil
	}
	if err := s.backend.CreateBucket(ctx, bucket); err != nil {
		return err
	}
	s.backendReady.Store(bucket, struct{}{})
	return nil
}

func (s *Server) invalidateBackendBucket(bucket string) {
	s.backendReady.Delete(bucket)
}

func objectMetadata(r *http.Request) store.ObjectMetadata {
	meta := store.ObjectMetadata{
		ContentType:        r.Header.Get("Content-Type"),
		ContentEncoding:    cleanContentEncoding(r.Header.Get("Content-Encoding")),
		ContentDisposition: r.Header.Get("Content-Disposition"),
		CacheControl:       r.Header.Get("Cache-Control"),
		Expires:            r.Header.Get("Expires"),
		User:               map[string]string{},
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			meta.User[strings.TrimPrefix(lower, "x-amz-meta-")] = strings.Join(values, ",")
		}
	}
	return meta
}

func cleanContentEncoding(value string) string {
	var encodings []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" || strings.EqualFold(part, "aws-chunked") {
			continue
		}
		encodings = append(encodings, part)
	}
	return strings.Join(encodings, ", ")
}

func storageClass(r *http.Request) string {
	if v := r.Header.Get("x-amz-storage-class"); v != "" {
		return v
	}
	return "STANDARD"
}

func md5Hex(body []byte) string {
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

func quotedETag(etag string) string {
	etag = strings.Trim(etag, `"`)
	if etag == "" {
		return `""`
	}
	return `"` + etag + `"`
}

func validateContentMD5(r *http.Request, body []byte) *s3err.Error {
	raw := r.Header.Get("Content-MD5")
	if raw == "" {
		return nil
	}
	got, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return s3err.New("InvalidDigest", "The Content-MD5 you specified is not valid.", http.StatusBadRequest)
	}
	sum := md5.Sum(body)
	if len(got) != len(sum) || !equalBytes(got, sum[:]) {
		return s3err.New("BadDigest", "The Content-MD5 you specified did not match what we received.", http.StatusBadRequest)
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func xmlBytes(w http.ResponseWriter, status int, v any) *s3err.Error {
	b, err := xml.Marshal(v)
	if err != nil {
		return s3err.Internal(err)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(b)
	return nil
}

func s3Time(nanos int64) string {
	return time.Unix(0, nanos).UTC().Format("2006-01-02T15:04:05.000Z")
}

func parsePositiveInt(s string, def int, max int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || (max > 0 && n > max) {
		return 0, errors.New("invalid integer")
	}
	return n, nil
}

func requestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path}
	return u.String()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	counts, err := s.store.VersionCounts(r.Context(), bucket)
	if err != nil {
		http.Error(w, "status unavailable", http.StatusInternalServerError)
		return
	}
	age, hasAge, err := s.store.OldestIncompleteAge(r.Context(), bucket)
	if err != nil {
		http.Error(w, "status unavailable", http.StatusInternalServerError)
		return
	}
	records, err := s.store.RecentReclaims(r.Context(), bucket, 20)
	if err != nil {
		http.Error(w, "status unavailable", http.StatusInternalServerError)
		return
	}
	type reclaimView struct {
		UploadID    string `json:"upload_id"`
		Bytes       int64  `json:"bytes"`
		Reason      string `json:"reason"`
		Status      string `json:"status"`
		ReclaimedAt string `json:"reclaimed_at"`
	}
	recent := make([]reclaimView, 0, len(records))
	for _, rec := range records {
		recent = append(recent, reclaimView{
			UploadID: rec.SessionID, Bytes: rec.Bytes, Reason: rec.Reason,
			Status: rec.Status, ReclaimedAt: time.Unix(0, rec.ReclaimedAt).UTC().Format(time.RFC3339Nano),
		})
	}
	out := struct {
		Bucket                         string        `json:"bucket"`
		CurrentVersions                int64         `json:"current_versions"`
		HistoricalVersions             int64         `json:"historical_versions"`
		HasIncompleteSessions          bool          `json:"has_incomplete_sessions"`
		OldestIncompleteSessionAgeSecs int64         `json:"oldest_incomplete_session_age_seconds"`
		RecentReclaims                 []reclaimView `json:"recent_reclaims"`
	}{
		Bucket:                         bucket,
		CurrentVersions:                counts.Current,
		HistoricalVersions:             counts.Historical,
		HasIncompleteSessions:          hasAge,
		OldestIncompleteSessionAgeSecs: int64(age / time.Second),
		RecentReclaims:                 recent,
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

func formatInt(n int) string { return strconv.Itoa(n) }
