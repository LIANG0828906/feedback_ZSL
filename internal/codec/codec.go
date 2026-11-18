package codec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/klauspost/reedsolomon"
)

const (
	DataShards   = 4
	ParityShards = 2
	TotalShards  = DataShards + ParityShards
)

type Codec struct {
	enc reedsolomon.Encoder
}

func New() (*Codec, error) {
	enc, err := reedsolomon.New(DataShards, ParityShards, reedsolomon.WithMaxGoroutines(1))
	if err != nil {
		return nil, err
	}
	return &Codec{enc: enc}, nil
}

// Encode splits one logical stripe into four equal data shards, pads the tail
// with zero bytes, and returns four data plus two parity shards.
func (c *Codec) Encode(data []byte) ([][]byte, int64, error) {
	if len(data) == 0 {
		return nil, 0, errors.New("empty stripe")
	}
	shardLen := (len(data) + DataShards - 1) / DataShards
	shards := make([][]byte, TotalShards)
	for i := 0; i < DataShards; i++ {
		shards[i] = make([]byte, shardLen)
		start := i * shardLen
		if start >= len(data) {
			continue
		}
		end := start + shardLen
		if end > len(data) {
			end = len(data)
		}
		copy(shards[i], data[start:end])
	}
	for i := DataShards; i < TotalShards; i++ {
		shards[i] = make([]byte, shardLen)
	}
	if err := c.enc.Encode(shards); err != nil {
		return nil, 0, fmt.Errorf("reed-solomon encode: %w", err)
	}
	return shards, int64(shardLen), nil
}

func (c *Codec) Reconstruct(shards [][]byte, present []bool) error {
	if len(shards) != TotalShards {
		return fmt.Errorf("expected %d shards, got %d", TotalShards, len(shards))
	}
	if len(present) != TotalShards {
		return fmt.Errorf("present mask has %d entries", len(present))
	}
	presentCount := 0
	for _, ok := range present {
		if ok {
			presentCount++
		}
	}
	if presentCount < DataShards {
		return fmt.Errorf("only %d shards available, need at least %d", presentCount, DataShards)
	}
	for i := range shards {
		if !present[i] {
			shards[i] = nil
		}
	}
	if err := c.enc.Reconstruct(shards); err != nil {
		return fmt.Errorf("reed-solomon reconstruct: %w", err)
	}
	return nil
}

func (c *Codec) Verify(shards [][]byte) (bool, error) {
	if len(shards) != TotalShards {
		return false, fmt.Errorf("expected %d shards, got %d", TotalShards, len(shards))
	}
	for i, s := range shards {
		if s == nil {
			return false, fmt.Errorf("shard %d is missing", i)
		}
	}
	return c.enc.Verify(shards)
}

func (c *Codec) LogicalData(shards [][]byte, length int64) ([]byte, error) {
	if len(shards) < DataShards {
		return nil, errors.New("not enough data shards")
	}
	var shardLen int
	for i := 0; i < DataShards; i++ {
		if len(shards[i]) == 0 {
			return nil, fmt.Errorf("data shard %d is empty", i)
		}
		if i == 0 {
			shardLen = len(shards[i])
		} else if len(shards[i]) != shardLen {
			return nil, fmt.Errorf("data shard lengths differ: %d != %d", len(shards[i]), shardLen)
		}
	}
	full := make([]byte, 0, shardLen*DataShards)
	for i := 0; i < DataShards; i++ {
		full = append(full, shards[i]...)
	}
	if length < 0 || length > int64(len(full)) {
		return nil, fmt.Errorf("invalid logical length %d (capacity %d)", length, len(full))
	}
	return full[:length], nil
}

func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
