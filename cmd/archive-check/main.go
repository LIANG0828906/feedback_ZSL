package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"archive/internal/seed"
)

type objectRow struct {
	ObjectID   string `json:"object_id"`
	Size       int64  `json:"size"`
	Seed       int64  `json:"seed"`
	DataSHA256 string `json:"data_sha256"`
}

type checkResult struct {
	Mode               string   `json:"mode"`
	ObjectsChecked     int      `json:"objects_checked"`
	FullObjectsChecked int      `json:"full_objects_checked"`
	RangesChecked      int      `json:"ranges_checked"`
	BytesRead          int64    `json:"bytes_read"`
	DegradedResponses  int      `json:"degraded_responses"`
	Failures           int      `json:"failures"`
	Unrecoverable      int      `json:"unrecoverable_responses"`
	Mismatches         []string `json:"mismatches"`
	ElapsedMS          int64    `json:"elapsed_ms"`
}

func main() {
	addr := flag.String("addr", "http://127.0.0.1:18080", "archive service base URL")
	mode := flag.String("mode", "healthy", "healthy/degraded expected read mode")
	ranges := flag.Int("ranges", 2, "additional deterministic ranges per object")
	outPath := flag.String("json", "", "write JSON result to this path")
	flag.Parse()

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Minute}
	start := time.Now()
	objects, err := fetchObjects(client, *addr)
	if err != nil {
		fatal(err)
	}
	res := checkResult{Mode: *mode, Mismatches: []string{}}
	for _, obj := range objects {
		if obj.Seed == 0 || obj.Size < 0 {
			continue
		}
		expected := seed.Generate(obj.Seed, obj.Size)
		expectedHash := hashHex(expected)
		if obj.DataSHA256 != "" && obj.DataSHA256 != expectedHash {
			res.Failures++
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s manifest_sha256 expected=%s service=%s", obj.ObjectID, expectedHash, obj.DataSHA256))
			continue
		}
		res.ObjectsChecked++
		body, degraded, code, status, err := fetchRange(client, *addr, obj.ObjectID, 0, obj.Size-1)
		if err != nil {
			res.Failures++
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s full read error=%v", obj.ObjectID, err))
			continue
		}
		if status != http.StatusOK && status != http.StatusPartialContent {
			if code == "STRIPE_UNRECOVERABLE" && *mode == "unrecoverable" {
				res.Unrecoverable++
				continue
			}
			res.Failures++
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s full read status=%d code=%s", obj.ObjectID, status, code))
			continue
		}
		if degraded {
			res.DegradedResponses++
		}
		res.FullObjectsChecked++
		res.BytesRead += int64(len(body))
		if got := hashHex(body); got != expectedHash {
			res.Failures++
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s full read digest got=%s want=%s", obj.ObjectID, got, expectedHash))
		}
		for n := 0; n < *ranges && obj.Size > 0; n++ {
			startOff := deterministicOffset(obj.Seed, obj.Size, n)
			length := int64(4096)
			if startOff+length > obj.Size {
				length = obj.Size - startOff
			}
			if length <= 0 {
				continue
			}
			part, deg, code, status, err := fetchRange(client, *addr, obj.ObjectID, startOff, startOff+length-1)
			if err != nil {
				res.Failures++
				res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s range %d-%d error=%v", obj.ObjectID, startOff, startOff+length-1, err))
				continue
			}
			if status != http.StatusPartialContent && status != http.StatusOK {
				if code == "STRIPE_UNRECOVERABLE" && *mode == "unrecoverable" {
					res.Unrecoverable++
					continue
				}
				res.Failures++
				res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s range status=%d code=%s", obj.ObjectID, status, code))
				continue
			}
			if deg {
				res.DegradedResponses++
			}
			res.RangesChecked++
			res.BytesRead += int64(len(part))
			want := hashHex(expected[startOff : startOff+length])
			if got := hashHex(part); got != want {
				res.Failures++
				res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s range %d-%d digest got=%s want=%s", obj.ObjectID, startOff, startOff+length-1, got, want))
			}
		}
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *outPath != "" {
		if err := os.WriteFile(*outPath, append(b, '\n'), 0o644); err != nil {
			fatal(err)
		}
	}
	if res.Failures > 0 {
		os.Exit(1)
	}
}

func deterministicOffset(seedValue, size int64, n int) int64 {
	if size <= 0 {
		return 0
	}
	x := uint64(seedValue)*0x9e3779b97f4a7c15 + uint64(n+1)*0xbf58476d1ce4e5b9
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	return int64(x % uint64(size))
}

func fetchObjects(client *http.Client, base string) ([]objectRow, error) {
	resp, err := client.Get(strings.TrimRight(base, "/") + "/v1/objects?limit=100000")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("objects status=%d body=%s", resp.StatusCode, b)
	}
	var envelope struct {
		Objects []objectRow `json:"objects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	return envelope.Objects, nil
}

func fetchRange(client *http.Client, base, id string, start, end int64) ([]byte, bool, string, int, error) {
	u := fmt.Sprintf("%s/v1/objects/%s?range=%d-%d", strings.TrimRight(base, "/"), url.PathEscape(id), start, end)
	resp, err := client.Get(u)
	if err != nil {
		return nil, false, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, "", resp.StatusCode, err
	}
	degraded := resp.Header.Get("X-Archive-Degraded") == "true"
	code := ""
	if resp.StatusCode >= 400 {
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &env)
		code = env.Error.Code
	}
	return body, degraded, code, resp.StatusCode, nil
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
