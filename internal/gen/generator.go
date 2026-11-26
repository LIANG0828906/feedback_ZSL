package gen

import (
	"encoding/json"
	"fmt"
	"math/rand"

	"enginelab.local/internal/loader"
	"enginelab.local/internal/model"
)

const DefaultSeed int64 = 20261127

type Bundle struct {
	Seed           int64                                   `json:"seed"`
	Project        *model.Project                          `json:"project"`
	Truth          *Truth                                  `json:"truth"`
	Docs           map[model.SchemaVersion]json.RawMessage `json:"docs"`
	SegmentFiles   map[string][]byte                       `json:"-"`
	CheckpointFile []byte                                  `json:"-"`
}

type generator struct {
	rng      *rand.Rand
	project  *model.Project
	nextLSN  uint64
	nextTick int64
	versions map[string]int64
	keys     []string
}

func NewBundle(seed int64) *Bundle {
	if seed == 0 {
		seed = DefaultSeed
	}
	g := &generator{
		rng:      rand.New(rand.NewSource(seed)),
		nextLSN:  1000,
		nextTick: 5000,
		versions: map[string]int64{},
		keys:     []string{"alpha", "bravo", "charlie", "delta"},
		project: &model.Project{
			ID:             fmt.Sprintf("wal-engineering-%d", seed),
			Seed:           seed,
			TickIntervalMS: 10,
			SchemaVersion:  model.SchemaV1,
		},
	}
	preds := [][]string{
		nil,
		{"seg-000"},
		{"seg-000"},
		{"seg-001"},
		{"seg-002"},
		{"seg-004"},
	}
	for i := 0; i < 6; i++ {
		seg := g.newSegment(i, preds[i])
		g.addStandardTransactions(seg, i)
		if i == 2 {
			g.addCheckpoint(seg)
		}
		g.project.Segments = append(g.project.Segments, seg)
	}
	g.project.LatestCheckpoint = g.project.SortedCheckpoints()[0]

	truth := BuildTruth(g.project)
	bundle := &Bundle{
		Seed:         seed,
		Project:      g.project,
		Truth:        truth,
		Docs:         map[model.SchemaVersion]json.RawMessage{},
		SegmentFiles: map[string][]byte{},
	}
	for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, err := loader.Encode(g.project, version)
		if err != nil {
			panic(err)
		}
		bundle.Docs[version] = data
	}
	for _, seg := range g.project.Segments {
		data, err := loader.EncodeSegmentFile(seg)
		if err != nil {
			panic(err)
		}
		bundle.SegmentFiles[seg.ID] = data
	}
	cpFile, err := loader.EncodeCheckpointFile(g.project.LatestCheckpoint)
	if err != nil {
		panic(err)
	}
	bundle.CheckpointFile = cpFile
	return bundle
}

func (g *generator) newSegment(index int, preds []string) *model.Segment {
	id := fmt.Sprintf("seg-%03d", index)
	base := g.nextLSN
	sizeKiB := int64(64 + g.rng.Intn(5)*64)
	return &model.Segment{
		ID:             id,
		Index:          index,
		BaseLSN:        base,
		FirstLSN:       base,
		SizeBytes:      sizeKiB * 1024,
		CreatedTick:    g.nextTick - 2,
		CreatedMS:      (g.nextTick - 2) * g.project.TickIntervalMS,
		PredecessorIDs: append([]string(nil), preds...),
		Integrity:      model.IntegrityComplete,
	}
}

func (g *generator) addRecord(seg *model.Segment, rec *model.Record) *model.Record {
	rec.SegmentID = seg.ID
	rec.Index = len(seg.Records)
	rec.LSN = g.nextLSN
	rec.Offset = g.nextLSN - seg.BaseLSN
	rec.Tick = g.nextTick
	rec.TimeMS = g.nextTick * g.project.TickIntervalMS
	rec.Complete = true
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("rec-%s-%03d", seg.ID, rec.Index+1)
	}
	seg.Records = append(seg.Records, rec)
	seg.CompleteRecords = len(seg.Records)
	seg.RawRecordCount = len(seg.Records)
	seg.LastLSN = rec.LSN
	g.nextLSN++
	g.nextTick += 3 + int64(g.rng.Intn(3))
	return rec
}

func (g *generator) addStandardTransactions(seg *model.Segment, si int) {
	put := func(suffix, key string, txKind string) {
		txID := fmt.Sprintf("tx-%s-%s", seg.ID, suffix)
		g.addRecord(seg, &model.Record{ID: fmt.Sprintf("rec-%s-%s-begin", seg.ID, suffix), TransactionID: txID, Op: model.OpBegin})
		version := g.versions[key] + 1
		g.versions[key] = version
		g.addRecord(seg, &model.Record{
			ID:            fmt.Sprintf("rec-%s-%s-put", seg.ID, suffix),
			TransactionID: txID,
			Op:            model.OpPut,
			Key:           key,
			Value:         fmt.Sprintf("v-seg%03d-%s-%d", si, suffix, version),
			Version:       version,
		})
		switch txKind {
		case "commit":
			g.addRecord(seg, &model.Record{ID: fmt.Sprintf("rec-%s-%s-commit", seg.ID, suffix), TransactionID: txID, Op: model.OpCommit})
		case "abort":
			g.addRecord(seg, &model.Record{ID: fmt.Sprintf("rec-%s-%s-abort", seg.ID, suffix), TransactionID: txID, Op: model.OpAbort})
		}
	}

	put("a", g.keys[si%len(g.keys)], "commit")
	put("b", g.keys[(si+1)%len(g.keys)], "commit")
	put("abort", g.keys[(si+2)%len(g.keys)], "abort")

	openTx := fmt.Sprintf("tx-%s-open", seg.ID)
	g.addRecord(seg, &model.Record{ID: fmt.Sprintf("rec-%s-open-begin", seg.ID), TransactionID: openTx, Op: model.OpBegin})
	openKey := g.keys[(si+3)%len(g.keys)]
	openVersion := g.versions[openKey] + 1
	g.versions[openKey] = openVersion
	g.addRecord(seg, &model.Record{
		ID:            fmt.Sprintf("rec-%s-open-put", seg.ID),
		TransactionID: openTx,
		Op:            model.OpPut,
		Key:           openKey,
		Value:         fmt.Sprintf("open-seg%03d-%d", si, openVersion),
		Version:       openVersion,
	})
}

func (g *generator) addCheckpoint(seg *model.Segment) {
	rec := g.addRecord(seg, &model.Record{
		ID: "rec-cp-1",
		Op: model.OpCheckpoint,
	})
	cp := &model.Checkpoint{
		ID:        "cp-1",
		RecordID:  rec.ID,
		LSN:       rec.LSN,
		SegmentID: seg.ID,
		AtMS:      rec.TimeMS,
		Sequence:  1,
	}
	g.project.Checkpoints = append(g.project.Checkpoints, cp)
}
