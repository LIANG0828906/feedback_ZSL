package sig

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"s3gw/internal/s3err"
)

type Version string

const (
	V2 Version = "v2"
	V4 Version = "v4"
)

type Result struct {
	Version     Version
	AccessKey   string
	PayloadHash string
	Presigned   bool
	Signature   string
	Scope       string
	Timestamp   string
	SigningKey  []byte
}

type Verifier struct {
	AccessKey string
	SecretKey string
	Region    string
	ClockSkew time.Duration
	Now       func() time.Time
}

func (v *Verifier) NowTime() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now().UTC()
}

func (v *Verifier) Authenticate(r *http.Request) (*Result, *s3err.Error) {
	q := r.URL.Query()
	if strings.EqualFold(q.Get("X-Amz-Algorithm"), "AWS4-HMAC-SHA256") {
		return v.authenticateV4Presigned(r)
	}
	if q.Get("AWSAccessKeyId") != "" && q.Get("Signature") != "" {
		return v.authenticateV2Presigned(r)
	}

	auth := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(auth, "AWS4-HMAC-SHA256 "):
		return v.authenticateV4Header(r, auth)
	case strings.HasPrefix(auth, "AWS "):
		return v.authenticateV2Header(r, auth)
	case auth == "":
		return nil, s3err.AccessDenied()
	default:
		e := s3err.New("AuthorizationHeaderMalformed", "The authorization header is malformed.", http.StatusBadRequest)
		return nil, e
	}
}

func (v *Verifier) VerifyPayload(result *Result, r *http.Request, body []byte) ([]byte, *s3err.Error) {
	if result == nil {
		return nil, s3err.AccessDenied()
	}
	h := result.PayloadHash
	if result.Version != V4 || h == "" || h == "UNSIGNED-PAYLOAD" {
		return body, nil
	}
	if strings.HasPrefix(h, "STREAMING-") {
		var (
			decoded []byte
			err     *s3err.Error
		)
		switch h {
		case "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER":
			decoded, err = decodeSignedChunks(body, result)
		case "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "STREAMING-UNSIGNED-PAYLOAD":
			decoded, err = decodeUnsignedChunks(body)
		default:
			return nil, s3err.NotImplemented()
		}
		if err != nil {
			return nil, err
		}
		if want := r.Header.Get("x-amz-decoded-content-length"); want != "" {
			n, parseErr := strconv.ParseInt(want, 10, 64)
			if parseErr != nil || n != int64(len(decoded)) {
				return nil, s3err.New("IncompleteBody", "The decoded content length does not match the x-amz-decoded-content-length header.", http.StatusBadRequest)
			}
		}
		return decoded, nil
	}
	expected, err := hex.DecodeString(h)
	if err != nil || len(expected) != sha256.Size {
		return nil, s3err.New("XAmzContentSHA256Mismatch", "The provided x-amz-content-sha256 header is invalid.", http.StatusBadRequest)
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare(sum[:], expected) != 1 {
		return nil, s3err.New("XAmzContentSHA256Mismatch", "The provided x-amz-content-sha256 does not match the request body.", http.StatusBadRequest)
	}
	return body, nil
}

func (v *Verifier) authenticateV2Header(r *http.Request, auth string) (*Result, *s3err.Error) {
	fields := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(auth, "AWS ")), ":", 2)
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The authorization header is malformed.", http.StatusBadRequest)
	}
	access, got := fields[0], fields[1]
	if access != v.AccessKey {
		return nil, s3err.InvalidAccessKeyID()
	}
	resource, query, queryErr := canonicalResourceV2(r)
	if queryErr != nil {
		return nil, s3err.InvalidRequest()
	}
	canonicalHeaders := canonicalAmzHeadersV2(r.Header)
	date := r.Header.Get("Date")
	if date == "" && r.Header.Get("x-amz-date") == "" {
		return nil, s3err.New("MissingSecurityHeader", "Your request was missing a required header: Date.", http.StatusBadRequest)
	}
	// MinIO follows S3 and uses Date. Some old SDK forks only send x-amz-date;
	// trying that date as a second candidate keeps those clients working.
	dates := []string{date}
	if xdate := r.Header.Get("x-amz-date"); xdate != "" && xdate != date {
		dates = append(dates, xdate)
	}
	var lastErr *s3err.Error
	for _, d := range dates {
		stringToSign := strings.Join([]string{
			r.Method,
			r.Header.Get("Content-MD5"),
			r.Header.Get("Content-Type"),
			d,
			canonicalHeaders,
		}, "\n") + canonicalResourceString(resource, query)
		expected := signV2(v.SecretKey, stringToSign)
		if hmac.Equal([]byte(expected), []byte(got)) {
			if err := v.checkV2Time(d); err != nil {
				lastErr = err
				continue
			}
			return &Result{Version: V2, AccessKey: access}, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, s3err.SignatureDoesNotMatch()
}

func (v *Verifier) authenticateV2Presigned(r *http.Request) (*Result, *s3err.Error) {
	q := r.URL.Query()
	access := q.Get("AWSAccessKeyId")
	got := q.Get("Signature")
	expires := q.Get("Expires")
	if access == "" || got == "" || expires == "" {
		return nil, s3err.New("AuthorizationQueryParametersError", "The authorization query parameters are invalid.", http.StatusBadRequest)
	}
	if access != v.AccessKey {
		return nil, s3err.InvalidAccessKeyID()
	}
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return nil, s3err.New("AuthorizationQueryParametersError", "The Expires parameter is invalid.", http.StatusBadRequest)
	}
	if v.NowTime().Unix() > exp {
		return nil, s3err.New("AccessDenied", "Request has expired.", http.StatusForbidden)
	}
	resource, query, queryErr := canonicalResourceV2(r)
	if queryErr != nil {
		return nil, s3err.InvalidRequest()
	}
	filtered := make([]string, 0, len(query))
	for _, kv := range query {
		if strings.HasPrefix(kv, "AWSAccessKeyId=") || strings.HasPrefix(kv, "Signature=") || strings.HasPrefix(kv, "Expires=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	stringToSign := strings.Join([]string{
		r.Method,
		r.Header.Get("Content-MD5"),
		r.Header.Get("Content-Type"),
		expires,
		canonicalAmzHeadersV2(r.Header),
	}, "\n") + canonicalResourceString(resource, filtered)
	expected := signV2(v.SecretKey, stringToSign)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(got)) != 1 {
		return nil, s3err.SignatureDoesNotMatch()
	}
	return &Result{Version: V2, AccessKey: access, Presigned: true}, nil
}

func (v *Verifier) checkV2Time(date string) *s3err.Error {
	if v.ClockSkew <= 0 || date == "" {
		return nil
	}
	t, err := http.ParseTime(date)
	if err != nil {
		if t, err = time.Parse("20060102T150405Z", date); err != nil {
			return s3err.New("AccessDenied", "The request date is invalid.", http.StatusForbidden)
		}
	}
	if d := v.NowTime().Sub(t); d > v.ClockSkew || d < -v.ClockSkew {
		return s3err.New("RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.", http.StatusForbidden)
	}
	return nil
}

type v4Credential struct {
	AccessKey string
	Date      string
	Region    string
	Service   string
	Term      string
}

func (v *Verifier) authenticateV4Header(r *http.Request, auth string) (*Result, *s3err.Error) {
	rest := strings.TrimSpace(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "))
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			fields[kv[0]] = kv[1]
		}
	}
	cred, err := parseCredential(fields["Credential"])
	if err != nil {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The authorization header is malformed.", http.StatusBadRequest)
	}
	if cred.AccessKey != v.AccessKey {
		return nil, s3err.InvalidAccessKeyID()
	}
	if cred.Service != "s3" || cred.Term != "aws4_request" {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The credential scope is invalid.", http.StatusBadRequest)
	}
	if cred.Region != v.Region {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The credential region does not match the configured region.", http.StatusBadRequest)
	}
	if fields["Signature"] == "" || fields["SignedHeaders"] == "" {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The authorization header is malformed.", http.StatusBadRequest)
	}
	timestamp := r.Header.Get("x-amz-date")
	if timestamp == "" {
		return nil, s3err.New("MissingSecurityHeader", "Your request was missing a required header: x-amz-date.", http.StatusBadRequest)
	}
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		return nil, s3err.New("MissingSecurityHeader", "Your request was missing a required header: x-amz-content-sha256.", http.StatusBadRequest)
	}
	if err := v.checkV4Time(timestamp); err != nil {
		return nil, err
	}
	if len(timestamp) < 8 || cred.Date != timestamp[:8] {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The credential date does not match x-amz-date.", http.StatusBadRequest)
	}
	signed := normalizeSignedHeaders(fields["SignedHeaders"])
	if len(signed) == 0 {
		return nil, s3err.New("AuthorizationHeaderMalformed", "The signed headers list is empty.", http.StatusBadRequest)
	}
	if !contains(signed, "host") || !contains(signed, "x-amz-date") || !contains(signed, "x-amz-content-sha256") {
		return nil, s3err.New("MissingSecurityHeader", "The signed headers must include host, x-amz-date and x-amz-content-sha256.", http.StatusBadRequest)
	}
	canonical, canonErr := canonicalRequestV4(r, signed, payloadHash, false)
	if canonErr != nil {
		return nil, s3err.InvalidRequest()
	}
	scope := strings.Join([]string{cred.Date, cred.Region, cred.Service, cred.Term}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		timestamp,
		scope,
		hex.EncodeToString(sha256Bytes([]byte(canonical))),
	}, "\n")
	key := signingKey(v.SecretKey, cred.Date, cred.Region, cred.Service)
	expected := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(fields["Signature"]))) != 1 {
		return nil, s3err.SignatureDoesNotMatch()
	}
	return &Result{
		Version: V4, AccessKey: cred.AccessKey, PayloadHash: payloadHash,
		Signature: strings.ToLower(fields["Signature"]), Scope: scope, Timestamp: timestamp, SigningKey: key,
	}, nil
}

func (v *Verifier) authenticateV4Presigned(r *http.Request) (*Result, *s3err.Error) {
	q := r.URL.Query()
	cred, err := parseCredential(q.Get("X-Amz-Credential"))
	if err != nil {
		return nil, s3err.New("AuthorizationQueryParametersError", "The authorization query parameters are invalid.", http.StatusBadRequest)
	}
	if cred.AccessKey != v.AccessKey {
		return nil, s3err.InvalidAccessKeyID()
	}
	if cred.Service != "s3" || cred.Term != "aws4_request" {
		return nil, s3err.New("AuthorizationQueryParametersError", "The credential scope is invalid.", http.StatusBadRequest)
	}
	if cred.Region != v.Region {
		return nil, s3err.New("AuthorizationQueryParametersError", "The credential region does not match the configured region.", http.StatusBadRequest)
	}
	timestamp := q.Get("X-Amz-Date")
	if timestamp == "" || q.Get("X-Amz-SignedHeaders") == "" || q.Get("X-Amz-Signature") == "" {
		return nil, s3err.New("AuthorizationQueryParametersError", "The authorization query parameters are invalid.", http.StatusBadRequest)
	}
	if err := v.checkV4Time(timestamp); err != nil {
		return nil, err
	}
	if len(timestamp) < 8 || cred.Date != timestamp[:8] {
		return nil, s3err.New("AuthorizationQueryParametersError", "The credential date does not match X-Amz-Date.", http.StatusBadRequest)
	}
	expires, convErr := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if convErr != nil || expires < 0 || expires > 7*24*3600 {
		return nil, s3err.New("AuthorizationQueryParametersError", "X-Amz-Expires is invalid.", http.StatusBadRequest)
	}
	t, _ := time.Parse("20060102T150405Z", timestamp)
	if v.NowTime().After(t.Add(time.Duration(expires) * time.Second)) {
		return nil, s3err.New("AccessDenied", "Request has expired.", http.StatusForbidden)
	}
	signed := normalizeSignedHeaders(q.Get("X-Amz-SignedHeaders"))
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	canonical, canonErr := canonicalRequestV4(r, signed, payloadHash, true)
	if canonErr != nil {
		return nil, s3err.InvalidRequest()
	}
	scope := strings.Join([]string{cred.Date, cred.Region, cred.Service, cred.Term}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		timestamp,
		scope,
		hex.EncodeToString(sha256Bytes([]byte(canonical))),
	}, "\n")
	key := signingKey(v.SecretKey, cred.Date, cred.Region, cred.Service)
	expected := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(q.Get("X-Amz-Signature")))) != 1 {
		return nil, s3err.SignatureDoesNotMatch()
	}
	return &Result{
		Version: V4, AccessKey: cred.AccessKey, PayloadHash: payloadHash, Presigned: true,
		Signature: strings.ToLower(q.Get("X-Amz-Signature")), Scope: scope, Timestamp: timestamp, SigningKey: key,
	}, nil
}

func decodeSignedChunks(body []byte, result *Result) ([]byte, *s3err.Error) {
	if len(result.SigningKey) == 0 || result.Signature == "" || result.Scope == "" || result.Timestamp == "" {
		return nil, s3err.InvalidRequest()
	}
	emptyHash := hex.EncodeToString(sha256Bytes(nil))
	previous := result.Signature
	pos := 0
	out := make([]byte, 0, len(body))
	for {
		line, next, err := readChunkLine(body, pos)
		if err != nil {
			return nil, s3err.New("IncompleteBody", "The aws-chunked request body is incomplete.", http.StatusBadRequest)
		}
		size, signature, err := parseSignedChunkHeader(line)
		if err != nil || signature == "" {
			return nil, s3err.New("InvalidRequest", "The aws-chunked request body is malformed.", http.StatusBadRequest)
		}
		pos = next
		if size < 0 || size > int64(len(body)-pos) {
			return nil, s3err.New("IncompleteBody", "The aws-chunked request body is incomplete.", http.StatusBadRequest)
		}
		data := body[pos : pos+int(size)]
		payloadHash := hex.EncodeToString(sha256Bytes(data))
		stringToSign := strings.Join([]string{
			"AWS4-HMAC-SHA256-PAYLOAD",
			result.Timestamp,
			result.Scope,
			previous,
			emptyHash,
			payloadHash,
		}, "\n")
		expected := hex.EncodeToString(hmacSHA256(result.SigningKey, []byte(stringToSign)))
		if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(signature))) != 1 {
			return nil, s3err.SignatureDoesNotMatch()
		}
		previous = expected
		pos += int(size)
		if size == 0 {
			return out, nil
		}
		out = append(out, data...)
		if pos+2 > len(body) || body[pos] != '\r' || body[pos+1] != '\n' {
			return nil, s3err.New("IncompleteBody", "The aws-chunked request body is incomplete.", http.StatusBadRequest)
		}
		pos += 2
	}
}

func decodeUnsignedChunks(body []byte) ([]byte, *s3err.Error) {
	pos := 0
	out := make([]byte, 0, len(body))
	for {
		line, next, err := readChunkLine(body, pos)
		if err != nil {
			return nil, s3err.New("IncompleteBody", "The aws-chunked request body is incomplete.", http.StatusBadRequest)
		}
		sizeText := line
		if idx := strings.IndexByte(sizeText, ';'); idx >= 0 {
			sizeText = sizeText[:idx]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 || size > int64(len(body)-next) {
			return nil, s3err.New("InvalidRequest", "The aws-chunked request body is malformed.", http.StatusBadRequest)
		}
		pos = next
		data := body[pos : pos+int(size)]
		if size == 0 {
			return out, nil
		}
		out = append(out, data...)
		pos += int(size)
		if pos+2 > len(body) || body[pos] != '\r' || body[pos+1] != '\n' {
			return nil, s3err.New("IncompleteBody", "The aws-chunked request body is incomplete.", http.StatusBadRequest)
		}
		pos += 2
	}
}

func readChunkLine(body []byte, pos int) (string, int, error) {
	if pos < 0 || pos >= len(body) {
		return "", pos, errors.New("missing chunk header")
	}
	idx := bytes.Index(body[pos:], []byte("\r\n"))
	if idx < 0 {
		return "", pos, errors.New("missing chunk header terminator")
	}
	return string(body[pos : pos+idx]), pos + idx + 2, nil
}

func parseSignedChunkHeader(line string) (int64, string, error) {
	parts := strings.Split(line, ";")
	if len(parts) < 2 {
		return 0, "", errors.New("missing chunk signature")
	}
	size, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 16, 64)
	if err != nil || size < 0 {
		return 0, "", errors.New("invalid chunk size")
	}
	signature := ""
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && key == "chunk-signature" {
			signature = strings.TrimSpace(value)
		}
	}
	return size, signature, nil
}

func (v *Verifier) checkV4Time(timestamp string) *s3err.Error {
	t, err := time.Parse("20060102T150405Z", timestamp)
	if err != nil {
		return s3err.New("AuthorizationHeaderMalformed", "x-amz-date is invalid.", http.StatusBadRequest)
	}
	if v.ClockSkew > 0 {
		if d := v.NowTime().Sub(t); d > v.ClockSkew || d < -v.ClockSkew {
			return s3err.New("RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.", http.StatusForbidden)
		}
	}
	return nil
}

func parseCredential(s string) (v4Credential, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 {
		return v4Credential{}, fmt.Errorf("invalid credential")
	}
	return v4Credential{AccessKey: parts[0], Date: parts[1], Region: parts[2], Service: parts[3], Term: parts[4]}, nil
}

func canonicalRequestV4(r *http.Request, signed []string, payloadHash string, presigned bool) (string, error) {
	var headers strings.Builder
	for _, name := range signed {
		var values []string
		if name == "host" {
			values = []string{r.Host}
		} else {
			values = r.Header.Values(name)
		}
		if len(values) == 0 {
			return "", fmt.Errorf("signed header %s is absent", name)
		}
		cleaned := make([]string, 0, len(values))
		for _, value := range values {
			cleaned = append(cleaned, collapseSpaces(strings.TrimSpace(value)))
		}
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(strings.Join(cleaned, ","))
		headers.WriteByte('\n')
	}
	uri := r.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	return strings.Join([]string{
		r.Method,
		uri,
		canonicalQueryV4(r.URL.Query(), presigned),
		headers.String(),
		strings.Join(signed, ";"),
		payloadHash,
	}, "\n"), nil
}

func canonicalQueryV4(values url.Values, excludeSignature bool) string {
	type pair struct{ k, v string }
	pairs := make([]pair, 0)
	for k, vs := range values {
		if excludeSignature && strings.EqualFold(k, "X-Amz-Signature") {
			continue
		}
		for _, val := range vs {
			pairs = append(pairs, pair{k: awsEncode(k), v: awsEncode(val)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func awsEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func normalizeSignedHeaders(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func contains(ss []string, s string) bool {
	for _, item := range ss {
		if item == s {
			return true
		}
	}
	return false
}

func signingKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func signV2(secret, stringToSign string) string {
	h := hmac.New(sha1.New, []byte(secret))
	_, _ = h.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

var v2ResourceList = []string{
	"acl", "cors", "delete", "encryption", "legal-hold", "lifecycle", "location",
	"logging", "notification", "partNumber", "policy", "requestPayment",
	"response-cache-control", "response-content-disposition", "response-content-encoding",
	"response-content-language", "response-content-type", "response-expires", "retention",
	"select", "select-type", "tagging", "torrent", "uploadId", "uploads", "versionId",
	"versioning", "versions", "website",
}

func canonicalResourceV2(r *http.Request) (string, []string, error) {
	resource := r.URL.EscapedPath()
	if resource == "" {
		resource = "/"
	}
	parts := strings.Split(r.URL.RawQuery, "&")
	values := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		decoded, err := url.QueryUnescape(p)
		if err != nil {
			return "", nil, err
		}
		values = append(values, decoded)
	}
	return resource, values, nil
}

func canonicalResourceString(resource string, query []string) string {
	keyval := map[string]string{}
	for _, item := range query {
		k, val, _ := strings.Cut(item, "=")
		keyval[k] = val
	}
	out := make([]string, 0)
	for _, key := range v2ResourceList {
		val, ok := keyval[key]
		if !ok {
			continue
		}
		if val == "" {
			out = append(out, key)
		} else {
			out = append(out, key+"="+val)
		}
	}
	if len(out) == 0 {
		return resource
	}
	return resource + "?" + strings.Join(out, "&")
}

func canonicalAmzHeadersV2(h http.Header) string {
	keys := make([]string, 0)
	for key := range h {
		lkey := strings.ToLower(key)
		if strings.HasPrefix(lkey, "x-amz-") {
			keys = append(keys, lkey)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+":"+strings.Join(h.Values(key), ","))
	}
	return strings.Join(lines, "\n")
}

func EqualBase64(a, b string) bool {
	return bytes.Equal([]byte(a), []byte(b))
}
