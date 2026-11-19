package gateway

import (
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"s3gw/internal/store"

	"s3gw/internal/s3err"
)

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if !validBucketName(bucket) {
		return s3err.New("InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest)
	}
	exists, err := s.store.BucketExists(r.Context(), bucket)
	if err != nil {
		return s3err.Internal(err)
	}
	if exists {
		return s3err.New("BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", http.StatusConflict)
	}
	if err := s.backend.CreateBucket(r.Context(), bucket); err != nil {
		return s3err.Wrap("InternalError", "Unable to create the backend bucket.", http.StatusInternalServerError, err)
	}
	s.backendReady.Store(bucket, struct{}{})
	if _, err := s.store.EnsureBucket(r.Context(), bucket); err != nil {
		_ = s.backend.DeleteBucket(r.Context(), bucket)
		s.backendReady.Delete(bucket)
		return s3err.Internal(err)
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) headBucket(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) putBucketVersioning(w http.ResponseWriter, r *http.Request, body []byte, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	var config versioningConfiguration
	if len(body) > 0 {
		if err := xml.Unmarshal(body, &config); err != nil {
			return s3err.MalformedXML()
		}
		if config.Status != "" && !strings.EqualFold(config.Status, "Enabled") {
			return s3err.New("NotImplemented", "This gateway keeps object versioning enabled.", http.StatusNotImplemented)
		}
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	return xmlBytes(w, http.StatusOK, versioningConfiguration{XMLNS: s3XMLNS, Status: "Enabled"})
}

func (s *Server) getBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	return xmlBytes(w, http.StatusOK, locationConstraint{XMLNS: s3XMLNS, Region: s.cfg.Region})
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	hasData, err := s.store.BucketHasData(r.Context(), bucket)
	if err != nil {
		return s3err.Internal(err)
	}
	if hasData {
		return s3err.BucketNotEmpty()
	}
	if err := s.backend.DeleteBucket(r.Context(), bucket); err != nil {
		return s3err.Wrap("InternalError", "Unable to delete the backend bucket.", http.StatusInternalServerError, err)
	}
	s.backendReady.Delete(bucket)
	if err := s.store.DeleteBucket(r.Context(), bucket); err != nil {
		return s3err.Internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, body []byte, bucket, key string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	if err := validateContentMD5(r, body); err != nil {
		return err
	}
	meta := objectMetadata(r)
	class := storageClass(r)
	acl := r.Header.Get("x-amz-acl")
	version, err := s.store.ReserveVersion(r.Context(), bucket, key, "object", meta, class, acl)
	if err != nil {
		return s3err.Internal(err)
	}
	if err := s.ensureBackendBucket(r.Context(), bucket); err != nil {
		s.store.FailVersion(r.Context(), version.ID)
		return s3err.Wrap("InternalError", "Unable to prepare the backend bucket.", http.StatusInternalServerError, err)
	}
	etag := md5Hex(body)
	if _, err := s.backend.PutObject(r.Context(), bucket, version.BackendKey, body, meta.ContentType); err != nil {
		s.store.FailVersion(r.Context(), version.ID)
		return s3err.Wrap("InternalError", "Unable to persist the object.", http.StatusInternalServerError, err)
	}
	if err := s.store.CommitVersion(r.Context(), version.ID, etag, int64(len(body)), "[]"); err != nil {
		_ = s.backend.DeleteObject(r.Context(), bucket, version.BackendKey)
		s.store.FailVersion(r.Context(), version.ID)
		return s3err.Internal(err)
	}
	w.Header().Set("ETag", quotedETag(etag))
	w.Header().Set("x-amz-version-id", version.VersionID)
	w.Header().Set("x-amz-storage-class", class)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string, head bool) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	q := r.URL.Query()
	var (
		version *store.Version
		err     error
	)
	if q.Has("versionId") {
		versionID := q.Get("versionId")
		if versionID == "" {
			return s3err.NoSuchVersion()
		}
		version, err = s.store.GetVersion(r.Context(), bucket, key, versionID)
		if err != nil {
			return s3err.Internal(err)
		}
		if version == nil || (version.State != store.VersionCommitted && version.State != store.VersionDeleting) {
			return s3err.NoSuchVersion()
		}
	} else {
		version, err = s.store.GetCurrentVersion(r.Context(), bucket, key)
		if err != nil {
			return s3err.Internal(err)
		}
		if version == nil {
			return s3err.NoSuchKey()
		}
	}
	if version.IsDeleteMarker() {
		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", version.VersionID)
		if q.Has("versionId") {
			return s3err.MethodNotAllowed()
		}
		return s3err.NoSuchKey()
	}
	if version.State == store.VersionDeleting {
		return s3err.NoSuchVersion()
	}

	meta := store.DecodeMetadata(version.MetadataJSON)
	var backendBody io.ReadCloser
	if !head {
		resp, berr := s.backend.OpenObject(r.Context(), bucket, version.BackendKey)
		if berr != nil {
			return s3err.Wrap("InternalError", "Unable to read the stored object.", http.StatusInternalServerError, berr)
		}
		backendBody = resp.Body
		defer backendBody.Close()
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(version.Size, 10))
	w.Header().Set("ETag", quotedETag(version.ETag))
	w.Header().Set("Last-Modified", time.Unix(0, version.CommittedAt).UTC().Format(http.TimeFormat))
	w.Header().Set("x-amz-version-id", version.VersionID)
	w.Header().Set("x-amz-storage-class", version.StorageClass)
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	if meta.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", meta.ContentEncoding)
	}
	if meta.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", meta.ContentDisposition)
	}
	if meta.CacheControl != "" {
		w.Header().Set("Cache-Control", meta.CacheControl)
	}
	if meta.Expires != "" {
		w.Header().Set("Expires", meta.Expires)
	}
	for name, value := range meta.User {
		w.Header().Set("x-amz-meta-"+name, value)
	}
	if head {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, backendBody)
	if copyErr != nil {
		s.logger.Printf("object stream failed version=%s: %v", version.VersionID, copyErr)
	}
	return nil
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	q := r.URL.Query()
	if q.Has("versionId") {
		versionID := q.Get("versionId")
		if versionID == "" {
			return s3err.NoSuchVersion()
		}
		version, already, err := s.store.BeginDeleteVersion(r.Context(), bucket, key, versionID)
		if err != nil {
			return s3err.Internal(err)
		}
		if version == nil {
			return s3err.NoSuchVersion()
		}
		if already {
			writeDeleteResponse(w, version)
			return nil
		}
		if version.BackendKey != "" {
			if err := s.backend.DeleteObject(r.Context(), bucket, version.BackendKey); err != nil {
				_ = s.store.CancelDeleteVersion(r.Context(), version.ID)
				return s3err.Wrap("InternalError", "Unable to delete the stored version.", http.StatusInternalServerError, err)
			}
		}
		if err := s.store.FinishDeleteVersion(r.Context(), version.ID); err != nil {
			return s3err.Internal(err)
		}
		writeDeleteResponse(w, version)
		return nil
	}

	marker, err := s.store.ReserveVersion(r.Context(), bucket, key, "delete", store.ObjectMetadata{}, "STANDARD", "")
	if err != nil {
		return s3err.Internal(err)
	}
	if err := s.store.CommitVersion(r.Context(), marker.ID, "", 0, "[]"); err != nil {
		s.store.FailVersion(r.Context(), marker.ID)
		return s3err.Internal(err)
	}
	w.Header().Set("x-amz-delete-marker", "true")
	w.Header().Set("x-amz-version-id", marker.VersionID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func writeDeleteResponse(w http.ResponseWriter, version *store.Version) {
	w.Header().Set("x-amz-version-id", version.VersionID)
	if version.IsDeleteMarker() {
		w.Header().Set("x-amz-delete-marker", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	q := r.URL.Query()
	max, err := parsePositiveInt(q.Get("max-keys"), 1000, s.cfg.MaxListKeys)
	if err != nil {
		return s3err.New("InvalidArgument", "max-keys is invalid.", http.StatusBadRequest)
	}
	prefix := q.Get("prefix")
	keyMarker := q.Get("key-marker")
	versionMarker := q.Get("version-id-marker")
	versions, truncated, err := s.store.ListVersions(r.Context(), bucket, prefix, keyMarker, versionMarker, max)
	if err != nil {
		return s3err.Internal(err)
	}
	latest, err := s.store.LatestVersionIDs(r.Context(), bucket)
	if err != nil {
		return s3err.Internal(err)
	}
	result := listVersionsResult{
		XMLNS:           s3XMLNS,
		Name:            bucket,
		Prefix:          prefix,
		KeyMarker:       keyMarker,
		VersionIDMarker: versionMarker,
		MaxKeys:         max,
		IsTruncated:     truncated,
	}
	for _, v := range versions {
		if v.IsDeleteMarker() {
			marker := listDeleteMarker{
				Key: v.Key, VersionID: v.VersionID, IsLatest: latest[v.Key] == v.VersionID,
				LastModified: s3Time(v.CommittedAt),
			}
			result.Entries = append(result.Entries, versionListEntry{Marker: &marker})
		} else {
			item := listVersionItem{
				Key: v.Key, VersionID: v.VersionID, IsLatest: latest[v.Key] == v.VersionID,
				LastModified: s3Time(v.CommittedAt), ETag: quotedETag(v.ETag),
				Size: v.Size, StorageClass: v.StorageClass,
			}
			result.Entries = append(result.Entries, versionListEntry{Version: &item})
		}
	}
	if truncated && len(versions) > 0 {
		last := versions[len(versions)-1]
		result.NextKeyMarker = last.Key
		result.NextVersionIDMarker = last.VersionID
	}
	return xmlBytes(w, http.StatusOK, result)
}

func (s *Server) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) *s3err.Error {
	if _, err := s.bucketExists(r.Context(), bucket); err != nil {
		return err
	}
	q := r.URL.Query()
	max, err := parsePositiveInt(q.Get("max-keys"), 1000, s.cfg.MaxListKeys)
	if err != nil {
		return s3err.New("InvalidArgument", "max-keys is invalid.", http.StatusBadRequest)
	}
	prefix := q.Get("prefix")
	startAfter := q.Get("start-after")
	if token := q.Get("continuation-token"); token != "" {
		startAfter = token
	}
	rows, truncated, err := s.store.ListCurrentObjects(r.Context(), bucket, prefix, startAfter, max)
	if err != nil {
		return s3err.Internal(err)
	}
	result := listBucketResult{
		XMLNS: s3XMLNS, Name: bucket, Prefix: prefix, StartAfter: startAfter,
		KeyCount: len(rows), MaxKeys: max, IsTruncated: truncated,
	}
	for _, row := range rows {
		result.Contents = append(result.Contents, listObjectEntry{
			Key: row.Key, LastModified: s3Time(row.CommittedAt), ETag: quotedETag(row.ETag),
			Size: row.Size, StorageClass: row.StorageClass,
		})
	}
	return xmlBytes(w, http.StatusOK, result)
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) *s3err.Error {
	names, err := s.store.ListBuckets(r.Context())
	if err != nil {
		return s3err.Internal(err)
	}
	sort.Strings(names)
	result := listAllMyBucketsResult{XMLNS: s3XMLNS}
	for _, name := range names {
		result.Buckets = append(result.Buckets, bucketXMLItem{Name: name, CreationDate: s3Time(time.Now().UnixNano())})
	}
	return xmlBytes(w, http.StatusOK, result)
}

func validBucketName(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 63 {
		return false
	}
	if strings.HasPrefix(bucket, "-") || strings.HasSuffix(bucket, "-") {
		return false
	}
	for _, r := range bucket {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return !strings.Contains(bucket, "..")
}
