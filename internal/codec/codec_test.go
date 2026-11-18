package codec

import (
	"bytes"
	"testing"
)

func TestEncodeReconstructTwoMissing(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 100003)
	for i := range data {
		data[i] = byte((i*31 + 7) % 251)
	}
	shards, shardLen, err := c.Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	if shardLen != 25001 {
		t.Fatalf("shard length=%d", shardLen)
	}
	present := []bool{true, true, false, true, false, true}
	shards[2] = nil
	shards[4] = nil
	if err := c.Reconstruct(shards, present); err != nil {
		t.Fatal(err)
	}
	ok, err := c.Verify(shards)
	if err != nil || !ok {
		t.Fatalf("verify ok=%v err=%v", ok, err)
	}
	got, err := c.LogicalData(shards, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed logical data differs")
	}
	if SHA256(got) != SHA256(data) {
		t.Fatal("digest differs")
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	c, _ := New()
	data := bytes.Repeat([]byte("archive"), 1000)
	shards, _, err := c.Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	shards[3][7] ^= 0xff
	ok, err := c.Verify(shards)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("corrupt data shard passed parity verification")
	}
}

func TestInsufficientShards(t *testing.T) {
	c, _ := New()
	shards, _, _ := c.Encode(make([]byte, 4096))
	present := []bool{true, true, true, false, false, false}
	shards[3], shards[4], shards[5] = nil, nil, nil
	if err := c.Reconstruct(shards, present); err == nil {
		t.Fatal("expected insufficient shard error")
	}
}
