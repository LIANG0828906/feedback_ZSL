package sig

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestStreamingSignedChunksDecodeAndVerify(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	result := &Result{
		Version: V4, PayloadHash: "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		Timestamp: "20261119T040000Z", Scope: "20261119/us-east-1/s3/aws4_request",
		Signature: strings.Repeat("1", 64), SigningKey: key,
	}
	body := makeSignedChunks(result, [][]byte{[]byte("hello "), []byte("streaming"), []byte(" world")})
	req, _ := http.NewRequest(http.MethodPut, "http://gateway.example/bucket/key", bytes.NewReader(body))
	req.Header.Set("x-amz-decoded-content-length", "21")
	v := &Verifier{}
	decoded, s3e := v.VerifyPayload(result, req, body)
	if s3e != nil {
		t.Fatalf("decode failed: %v", s3e)
	}
	if string(decoded) != "hello streaming world" {
		t.Fatalf("decoded=%q", decoded)
	}

	tampered := append([]byte{}, body...)
	idx := bytes.Index(tampered, []byte("streaming"))
	tampered[idx] = 'X'
	if _, s3e := v.VerifyPayload(result, req, tampered); s3e == nil || s3e.Code != "SignatureDoesNotMatch" {
		t.Fatalf("tampered chunk accepted: %v", s3e)
	}
}

func makeSignedChunks(result *Result, chunks [][]byte) []byte {
	emptyHash := hex.EncodeToString(sha256Bytes(nil))
	previous := result.Signature
	var out bytes.Buffer
	for _, chunk := range chunks {
		stringToSign := strings.Join([]string{
			"AWS4-HMAC-SHA256-PAYLOAD", result.Timestamp, result.Scope, previous,
			emptyHash, hex.EncodeToString(sha256Bytes(chunk)),
		}, "\n")
		signature := hex.EncodeToString(hmacSHA256(result.SigningKey, []byte(stringToSign)))
		out.WriteString(strconv.FormatInt(int64(len(chunk)), 16))
		out.WriteString(";chunk-signature=")
		out.WriteString(signature)
		out.WriteString("\r\n")
		out.Write(chunk)
		out.WriteString("\r\n")
		previous = signature
	}
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD", result.Timestamp, result.Scope, previous,
		emptyHash, emptyHash,
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(result.SigningKey, []byte(stringToSign)))
	fmt.Fprintf(&out, "0;chunk-signature=%s\r\n\r\n", signature)
	return out.Bytes()
}
