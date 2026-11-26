package gen

import (
	"math/rand"

	"meshpolicy/internal/model"
	"meshpolicy/internal/schema"
)

func mustEncodeV1(p *model.Project, rng *rand.Rand) []byte {
	b, err := schema.EncodeV1(p, rng)
	if err != nil {
		panic(err)
	}
	return b
}

func mustEncodeV2(p *model.Project, rng *rand.Rand) []byte {
	b, err := schema.EncodeV2(p, rng)
	if err != nil {
		panic(err)
	}
	return b
}

func mustEncodeV3(p *model.Project, rng *rand.Rand) []byte {
	b, err := schema.EncodeV3(p, rng)
	if err != nil {
		panic(err)
	}
	return b
}
