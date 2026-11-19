package sig

import (
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SignV2Request signs a header-authenticated S3 Signature V2 request.
func SignV2Request(r *http.Request, accessKey, secretKey string, now time.Time) {
	r.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	resource, query, _ := canonicalResourceV2(r)
	stringToSign := strings.Join([]string{
		r.Method,
		r.Header.Get("Content-MD5"),
		r.Header.Get("Content-Type"),
		r.Header.Get("Date"),
		canonicalAmzHeadersV2(r.Header),
	}, "\n") + canonicalResourceString(resource, query)
	signature := signV2(secretKey, stringToSign)
	r.Header.Set("Authorization", "AWS "+accessKey+":"+signature)
}

// SignV4Request signs a header-authenticated S3 Signature V4 request.
func SignV4Request(r *http.Request, accessKey, secretKey, region string, payload []byte, now time.Time) {
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	sum := sha256Bytes(payload)
	payloadHash := hex.EncodeToString(sum)
	r.Header.Set("x-amz-content-sha256", payloadHash)
	r.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if r.Header.Get("Content-Type") != "" {
		signed = append(signed, "content-type")
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") || strings.HasPrefix(lower, "x-amz-acl") ||
			strings.HasPrefix(lower, "x-amz-storage-class") {
			if !contains(signed, lower) {
				signed = append(signed, lower)
			}
		}
	}
	sort.Strings(signed)
	canonical, err := canonicalRequestV4(r, signed, payloadHash, false)
	if err != nil {
		return
	}
	scope := strings.Join([]string{now.UTC().Format("20060102"), region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		r.Header.Get("x-amz-date"),
		scope,
		hex.EncodeToString(sha256Bytes([]byte(canonical))),
	}, "\n")
	key := signingKey(secretKey, now.UTC().Format("20060102"), region, "s3")
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+strings.Join(signed, ";")+", Signature="+signature)
}

// SignV2Presigned adds a SigV2 presigned query to a request.
func SignV2Presigned(r *http.Request, accessKey, secretKey string, expires time.Time) {
	q := r.URL.Query()
	q.Set("AWSAccessKeyId", accessKey)
	q.Set("Expires", strconv.FormatInt(expires.Unix(), 10))
	r.URL.RawQuery = q.Encode()
	resource, query, _ := canonicalResourceV2(r)
	filtered := make([]string, 0, len(query))
	for _, kv := range query {
		if strings.HasPrefix(kv, "AWSAccessKeyId=") || strings.HasPrefix(kv, "Expires=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	stringToSign := strings.Join([]string{
		r.Method,
		r.Header.Get("Content-MD5"),
		r.Header.Get("Content-Type"),
		q.Get("Expires"),
		canonicalAmzHeadersV2(r.Header),
	}, "\n") + canonicalResourceString(resource, filtered)
	q.Set("Signature", signV2(secretKey, stringToSign))
	r.URL.RawQuery = q.Encode()
}

// SignV4Presigned adds a SigV4 presigned query to a request.
func SignV4Presigned(r *http.Request, accessKey, secretKey, region string, expires time.Duration, payloadHash string) {
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	now := time.Now().UTC()
	date := now.Format("20060102")
	timestamp := now.Format("20060102T150405Z")
	scope := strings.Join([]string{date, region, "s3", "aws4_request"}, "/")
	q := r.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+scope)
	q.Set("X-Amz-Date", timestamp)
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	r.URL.RawQuery = q.Encode()
	r.Header.Set("x-amz-content-sha256", payloadHash)
	canonical, err := canonicalRequestV4(r, []string{"host"}, payloadHash, true)
	if err != nil {
		return
	}
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		timestamp,
		scope,
		hex.EncodeToString(sha256Bytes([]byte(canonical))),
	}, "\n")
	key := signingKey(secretKey, date, region, "s3")
	q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(key, []byte(stringToSign))))
	r.URL.RawQuery = q.Encode()
}
