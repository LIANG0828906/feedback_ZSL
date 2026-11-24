package uuid

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

type UUID [16]byte

var Nil UUID

func New() UUID {
	var value UUID
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return value
}

func NewString() string {
	return New().String()
}

func NewSHA1(space UUID, data []byte) UUID {
	hash := sha1.New()
	hash.Write(space[:])
	hash.Write(data)
	sum := hash.Sum(nil)
	var value UUID
	copy(value[:], sum[:16])
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	return value
}

func Parse(input string) (UUID, error) {
	var value UUID
	text := strings.TrimSpace(input)
	if len(text) == 38 && text[0] == '{' && text[len(text)-1] == '}' {
		text = text[1 : len(text)-1]
	}
	if len(text) != 36 || text[8] != '-' || text[13] != '-' || text[18] != '-' || text[23] != '-' {
		return value, fmt.Errorf("uuid: invalid format")
	}
	compact := text[0:8] + text[9:13] + text[14:18] + text[19:23] + text[24:36]
	if _, err := hex.Decode(value[:], []byte(compact)); err != nil {
		return UUID{}, fmt.Errorf("uuid: invalid format: %w", err)
	}
	return value, nil
}

func MustParse(input string) UUID {
	value, err := Parse(input)
	if err != nil {
		panic(err)
	}
	return value
}

func (value UUID) String() string {
	var encoded [36]byte
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded[:])
}
