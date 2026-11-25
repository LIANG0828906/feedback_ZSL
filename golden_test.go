package thermalcache

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestFixedSeedGoldenDigest(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	res := runProject(t, p)
	digest := sha256.Sum256([]byte(normalizedResultJSON(t, res)))
	got := hex.EncodeToString(digest[:])
	const want = "39293d8436900a8fc2cfda02ffb4d8d1a10620f709a9d3665ca47ab7f9951495"
	t.Logf("fixed-seed result digest=%s", got)
	if want != "" && got != want {
		t.Fatalf("fixed-seed result drift: got %s want %s", got, want)
	}
	if res.Fingerprint != "23170f32ca4500abb3304d34074dc80e7ee9438671b5be5944e51627c160c42f" {
		t.Fatalf("fixture fingerprint drift: %s", res.Fingerprint)
	}
}
