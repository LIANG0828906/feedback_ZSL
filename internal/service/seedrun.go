package service

import (
	"context"
	"fmt"
	"time"

	seedsrc "archive/internal/seed"
)

type SeedObject struct {
	ObjectID   string `json:"object_id"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	Seed       int64  `json:"seed"`
	DataSHA256 string `json:"data_sha256"`
}

type SeedResult struct {
	Profile       string       `json:"profile"`
	SeedBase      int64        `json:"seed_base"`
	TotalObjects  int          `json:"total_objects"`
	Written       int          `json:"written"`
	Skipped       int          `json:"skipped"`
	SmallObjects  int          `json:"small_objects"`
	MediumObjects int          `json:"medium_objects"`
	SmallBytes    int64        `json:"small_bytes"`
	MediumBytes   int64        `json:"medium_bytes"`
	TotalBytes    int64        `json:"total_bytes"`
	ElapsedMS     int64        `json:"elapsed_ms"`
	Objects       []SeedObject `json:"objects"`
}

func (s *Service) Seed(ctx context.Context, profile string, seedBase int64, skipExisting bool) (SeedResult, error) {
	if profile == "" {
		profile = "full"
	}
	all := seedsrc.FullProfile(seedBase)
	specs := make([]seedsrc.Spec, 0, len(all))
	for _, sp := range all {
		switch profile {
		case "full":
			specs = append(specs, sp)
		case "small":
			if sp.Kind == "small" {
				specs = append(specs, sp)
			}
		case "medium":
			if sp.Kind == "medium" {
				specs = append(specs, sp)
			}
		default:
			return SeedResult{}, NewAppError("INVALID_PROFILE", "profile must be full, small, or medium")
		}
	}
	start := time.Now()
	res := SeedResult{
		Profile: profile, SeedBase: seedBase, TotalObjects: len(specs),
		Objects: make([]SeedObject, 0, len(specs)),
	}
	for _, sp := range specs {
		exists, err := s.redis.ObjectExists(ctx, sp.ID)
		if err != nil {
			return res, err
		}
		if exists {
			if skipExisting {
				res.Skipped++
				res.Objects = append(res.Objects, SeedObject{ObjectID: sp.ID, Kind: sp.Kind, Size: sp.Size, Seed: sp.Seed})
				continue
			}
			return res, NewAppError("OBJECT_EXISTS", fmt.Sprintf("object %s already exists", sp.ID))
		}
		data := seedsrc.Generate(sp.Seed, sp.Size)
		wr, err := s.WriteObject(ctx, sp.ID, data, sp.Seed)
		if err != nil {
			return res, fmt.Errorf("seed %s: %w", sp.ID, err)
		}
		res.Written++
		if sp.Kind == "small" {
			res.SmallObjects++
			res.SmallBytes += sp.Size
		} else {
			res.MediumObjects++
			res.MediumBytes += sp.Size
		}
		res.TotalBytes += sp.Size
		res.Objects = append(res.Objects, SeedObject{
			ObjectID: sp.ID, Kind: sp.Kind, Size: sp.Size, Seed: sp.Seed, DataSHA256: wr.DataSHA256,
		})
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

func (s *Service) ListObjects(ctx context.Context, limit int) ([]SeedObject, error) {
	objects, err := s.redis.ListObjects(ctx)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(objects) > limit {
		objects = objects[:limit]
	}
	out := make([]SeedObject, 0, len(objects))
	for _, o := range objects {
		kind := "small"
		if o.Size > 512<<10 {
			kind = "medium"
		}
		out = append(out, SeedObject{ObjectID: o.ID, Kind: kind, Size: o.Size, Seed: o.Seed, DataSHA256: o.DataSHA256})
	}
	return out, nil
}
