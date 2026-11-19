package gateway

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"

	"s3gw/internal/s3err"
	"s3gw/internal/store"
)

type uploadedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

func newUploadID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return s3err.RequestID()
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) initiateMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	if err := s.ensureBackendBucket(r.Context(), bucket); err != nil {
		return s3err.Wrap("InternalError", "Unable to prepare the backend bucket.", http.StatusInternalServerError, err)
	}
	meta := objectMetadata(r)
	class := storageClass(r)
	acl := r.Header.Get("x-amz-acl")
	uploadID := newUploadID()
	session, err := s.store.CreateSession(r.Context(), uploadID, bucket, key, meta, class, acl)
	if err != nil {
		return s3err.Internal(err)
	}
	result := initiateMultipartUploadResult{
		XMLNS: s3XMLNS, Bucket: session.Bucket, Key: session.Key, UploadID: session.UploadID,
	}
	return xmlBytes(w, http.StatusOK, result)
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, body []byte, bucket, key string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	partNumber, err := parsePartNumber(r.URL.Query().Get("partNumber"))
	if err != nil {
		return s3err.New("InvalidArgument", "Part number must be an integer between 1 and 10000.", http.StatusBadRequest)
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return s3err.NoSuchUpload()
	}
	if err := validateContentMD5(r, body); err != nil {
		return err
	}
	unlock := s.locks.Lock(uploadID)
	defer unlock()
	session, err := s.store.GetSession(r.Context(), uploadID)
	if err != nil {
		return s3err.Internal(err)
	}
	if session == nil || session.Bucket != bucket || session.Key != key {
		return s3err.NoSuchUpload()
	}
	if session.Status != store.SessionActive {
		return s3err.NoSuchUpload()
	}
	if err := s.ensureBackendBucket(r.Context(), bucket); err != nil {
		return s3err.Wrap("InternalError", "Unable to prepare the backend bucket.", http.StatusInternalServerError, err)
	}
	etag := md5Hex(body)
	backendKey := store.UploadPartBackendKey(uploadID, partNumber)
	if _, err := s.backend.PutObject(r.Context(), bucket, backendKey, body, "application/octet-stream"); err != nil {
		return s3err.Wrap("InternalError", "Unable to persist the uploaded part.", http.StatusInternalServerError, err)
	}
	if err := s.store.PutPart(r.Context(), session.ID, partNumber, etag, int64(len(body)), backendKey); err != nil {
		_ = s.backend.DeleteObject(r.Context(), bucket, backendKey)
		return s3err.Internal(err)
	}
	w.Header().Set("ETag", quotedETag(etag))
	w.WriteHeader(http.StatusOK)
	return nil
}

func parsePartNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 10000 {
		return 0, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, body []byte, bucket, key string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return s3err.NoSuchUpload()
	}
	if err := validateContentMD5(r, body); err != nil {
		return err
	}
	var request completeMultipartUpload
	if err := xml.Unmarshal(body, &request); err != nil || len(request.Parts) == 0 {
		return s3err.MalformedXML()
	}
	if len(request.Parts) > 10000 {
		return s3err.InvalidPartOrder()
	}
	lastPart := 0
	for _, p := range request.Parts {
		if p.PartNumber <= lastPart || p.PartNumber < 1 || p.PartNumber > 10000 {
			return s3err.InvalidPartOrder()
		}
		lastPart = p.PartNumber
	}

	unlock := s.locks.Lock(uploadID)
	defer unlock()
	session, err := s.store.GetSession(r.Context(), uploadID)
	if err != nil {
		return s3err.Internal(err)
	}
	if session == nil || session.Bucket != bucket || session.Key != key {
		return s3err.NoSuchUpload()
	}
	if session.Status != store.SessionActive {
		return s3err.NoSuchUpload()
	}
	claimed, err := s.store.ClaimSession(r.Context(), uploadID, store.SessionActive, store.SessionCompleting)
	if err != nil {
		return s3err.Internal(err)
	}
	if !claimed {
		return s3err.NoSuchUpload()
	}
	resetActive := func() {
		_ = s.store.SetSessionStatus(r.Context(), uploadID, store.SessionCompleting, store.SessionActive)
	}

	allParts, err := s.store.ListParts(r.Context(), session.ID)
	if err != nil {
		resetActive()
		return s3err.Internal(err)
	}
	partByNumber := make(map[int]store.Part, len(allParts))
	for _, p := range allParts {
		partByNumber[p.PartNumber] = p
	}
	var object bytes.Buffer
	manifest := make([]uploadedPart, 0, len(request.Parts))
	partDigests := make([][]byte, 0, len(request.Parts))
	for _, requested := range request.Parts {
		part, ok := partByNumber[requested.PartNumber]
		if !ok || !sameETag(requested.ETag, part.ETag) {
			resetActive()
			return s3err.InvalidPart()
		}
		data, berr := s.backend.GetObject(r.Context(), bucket, part.BackendKey)
		if berr != nil {
			resetActive()
			return s3err.Wrap("InvalidPart", "One or more parts could not be read from storage.", http.StatusBadRequest, berr)
		}
		if int64(len(data)) != part.Size {
			resetActive()
			return s3err.InvalidPart()
		}
		if md5Hex(data) != strings.ToLower(strings.Trim(part.ETag, `"`)) {
			resetActive()
			return s3err.InvalidPart()
		}
		if int64(object.Len()+len(data)) > s.cfg.MaxObjectBytes {
			resetActive()
			return s3err.EntityTooLarge()
		}
		object.Write(data)
		digest, _ := hex.DecodeString(md5Hex(data))
		partDigests = append(partDigests, digest)
		manifest = append(manifest, uploadedPart{
			PartNumber: part.PartNumber, ETag: strings.ToLower(strings.Trim(part.ETag, `"`)), Size: part.Size,
		})
	}
	etag := multipartETag(partDigests)
	manifestJSON, _ := json.Marshal(manifest)
	meta := store.DecodeMetadata(session.MetadataJSON)
	version, verr := s.store.ReserveVersion(r.Context(), bucket, key, "object", meta, session.StorageClass, session.ACL)
	if verr != nil {
		resetActive()
		return s3err.Internal(verr)
	}
	if err := s.ensureBackendBucket(r.Context(), bucket); err != nil {
		s.store.FailVersion(r.Context(), version.ID)
		resetActive()
		return s3err.Wrap("InternalError", "Unable to prepare the backend bucket.", http.StatusInternalServerError, err)
	}
	if _, err := s.backend.PutObject(r.Context(), bucket, version.BackendKey, object.Bytes(), meta.ContentType); err != nil {
		s.store.FailVersion(r.Context(), version.ID)
		resetActive()
		return s3err.Wrap("InternalError", "Unable to persist the completed object.", http.StatusInternalServerError, err)
	}
	if err := s.store.FinalizeMultipart(r.Context(), version.ID, etag, int64(object.Len()), string(manifestJSON), session.ID); err != nil {
		_ = s.backend.DeleteObject(r.Context(), bucket, version.BackendKey)
		s.store.FailVersion(r.Context(), version.ID)
		resetActive()
		return s3err.Internal(err)
	}
	for _, part := range allParts {
		if err := s.backend.DeleteObject(r.Context(), bucket, part.BackendKey); err != nil {
			s.logger.Printf("completed upload staging cleanup failed upload=%s part=%d: %v", uploadID, part.PartNumber, err)
		}
	}
	w.Header().Set("ETag", quotedETag(etag))
	w.Header().Set("x-amz-version-id", version.VersionID)
	result := completeMultipartUploadResult{
		XMLNS: s3XMLNS, Location: requestURL(r) + "?versionId=" + version.VersionID,
		Bucket: bucket, Key: key, ETag: quotedETag(etag),
	}
	return xmlBytes(w, http.StatusOK, result)
}

func multipartETag(digests [][]byte) string {
	var concat bytes.Buffer
	for _, d := range digests {
		concat.Write(d)
	}
	sum := md5.Sum(concat.Bytes())
	return hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(digests))
}

func sameETag(a, b string) bool {
	a = strings.ToLower(strings.Trim(strings.TrimSpace(a), `"`))
	b = strings.ToLower(strings.Trim(strings.TrimSpace(b), `"`))
	return a != "" && a == b
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) *s3err.Error {
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return s3err.NoSuchUpload()
	}
	unlock := s.locks.Lock(uploadID)
	defer unlock()
	session, err := s.store.GetSession(r.Context(), uploadID)
	if err != nil {
		return s3err.Internal(err)
	}
	if session == nil || session.Bucket != bucket || session.Key != key {
		return s3err.NoSuchUpload()
	}
	if session.Status == store.SessionAborted || session.Status == store.SessionReaped {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if session.Status == store.SessionAborting {
		// A previous abort attempt may have been interrupted; finish it.
	} else {
		if session.Status != store.SessionActive {
			return s3err.NoSuchUpload()
		}
		claimed, err := s.store.ClaimSession(r.Context(), uploadID, store.SessionActive, store.SessionAborting)
		if err != nil {
			return s3err.Internal(err)
		}
		if !claimed {
			return s3err.NoSuchUpload()
		}
		session.Status = store.SessionAborting
	}
	parts, err := s.store.ListParts(r.Context(), session.ID)
	if err != nil {
		return s3err.Internal(err)
	}
	var deleteFailed bool
	for _, part := range parts {
		if err := s.backend.DeleteObject(r.Context(), bucket, part.BackendKey); err != nil {
			s.logger.Printf("abort staging cleanup failed upload=%s part=%d: %v", uploadID, part.PartNumber, err)
			deleteFailed = true
		}
	}
	if deleteFailed {
		_ = s.store.SetSessionStatus(r.Context(), uploadID, store.SessionAborting, store.SessionActive)
		return s3err.New("InternalError", "Unable to remove all uploaded parts.", http.StatusInternalServerError)
	}
	if _, err := s.store.FinishAbort(r.Context(), session.ID, uploadID); err != nil {
		return s3err.Internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, bucket, key string) *s3err.Error {
	uploadID := r.URL.Query().Get("uploadId")
	session, err := s.store.GetSession(r.Context(), uploadID)
	if err != nil {
		return s3err.Internal(err)
	}
	if session == nil || session.Bucket != bucket || session.Key != key || session.Status != store.SessionActive {
		return s3err.NoSuchUpload()
	}
	parts, err := s.store.ListParts(r.Context(), session.ID)
	if err != nil {
		return s3err.Internal(err)
	}
	marker, convErr := parsePositiveInt(r.URL.Query().Get("part-number-marker"), 0, 10000)
	if convErr != nil {
		return s3err.New("InvalidArgument", "part-number-marker is invalid.", http.StatusBadRequest)
	}
	max, convErr := parsePositiveInt(r.URL.Query().Get("max-parts"), 1000, 1000)
	if convErr != nil {
		return s3err.New("InvalidArgument", "max-parts is invalid.", http.StatusBadRequest)
	}
	result := listPartsResult{
		XMLNS: s3XMLNS, Bucket: bucket, Key: key, UploadID: uploadID,
		PartNumberMarker: marker, MaxParts: max,
	}
	for _, part := range parts {
		if part.PartNumber <= marker {
			continue
		}
		if len(result.Part) >= max {
			result.IsTruncated = true
			break
		}
		result.Part = append(result.Part, listPartItem{
			PartNumber: part.PartNumber, LastModified: s3Time(part.CreatedAt),
			ETag: quotedETag(part.ETag), Size: part.Size,
		})
	}
	if len(result.Part) > 0 {
		result.NextPartNumberMarker = result.Part[len(result.Part)-1].PartNumber
	}
	return xmlBytes(w, http.StatusOK, result)
}

func (s *Server) listMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	max, convErr := parsePositiveInt(q.Get("max-uploads"), 1000, s.cfg.MaxListKeys)
	if convErr != nil {
		return s3err.New("InvalidArgument", "max-uploads is invalid.", http.StatusBadRequest)
	}
	keyMarker := q.Get("key-marker")
	uploads, truncated, err := s.store.ListIncompleteSessions(r.Context(), bucket, prefix, keyMarker, max)
	if err != nil {
		return s3err.Internal(err)
	}
	result := listMultipartUploadsResult{
		XMLNS: s3XMLNS, Bucket: bucket, Prefix: prefix, KeyMarker: keyMarker,
		MaxUploads: max, IsTruncated: truncated,
	}
	for _, session := range uploads {
		result.Uploads = append(result.Uploads, multipartUploadItem{
			Key: session.Key, UploadID: session.UploadID, StorageClass: session.StorageClass,
			Initiated: s3Time(session.CreatedAt),
		})
	}
	if truncated && len(uploads) > 0 {
		result.NextKeyMarker = uploads[len(uploads)-1].Key
		result.NextUploadIDMarker = uploads[len(uploads)-1].UploadID
	}
	return xmlBytes(w, http.StatusOK, result)
}
