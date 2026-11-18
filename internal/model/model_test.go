package model

import "testing"

func baseStripe() StripeMeta {
	s := StripeMeta{ID: "s1", ObjectID: "o1", Length: 100, ShardLength: 25, Blocks: make([]BlockMeta, BlockCount)}
	for i := 0; i < BlockCount; i++ {
		s.Blocks[i] = BlockMeta{Index: i, PrimaryPath: "p", PrimaryExists: true, PrimaryLength: 25}
	}
	return s
}

func allOnline() DirSet {
	var d DirSet
	for i := 0; i < BlockCount; i++ {
		d.Dirs[i] = DirState{Index: i, Online: true}
	}
	return d
}

func TestRefreshHealthy(t *testing.T) {
	s := baseStripe()
	s.Refresh(allOnline())
	if s.Status != StatusHealthy || s.AvailableBlocks != 6 || len(s.MissingBlocks) != 0 {
		t.Fatalf("unexpected: status=%s available=%d missing=%v", s.Status, s.AvailableBlocks, s.MissingBlocks)
	}
}

func TestRefreshRepairedOfflineDirs(t *testing.T) {
	s := baseStripe()
	dirs := allOnline()
	dirs.Dirs[0].Online = false
	dirs.Dirs[1].Online = false
	s.Blocks[0].BackupState = "committed"
	s.Blocks[0].BackupPath = "b"
	s.Blocks[1].BackupState = "committed"
	s.Blocks[1].BackupPath = "b"
	s.Refresh(dirs)
	if s.Status != StatusDegraded || s.AvailableBlocks != 6 || len(s.MissingBlocks) != 0 {
		t.Fatalf("unexpected: status=%s available=%d missing=%v reason=%s", s.Status, s.AvailableBlocks, s.MissingBlocks, s.DegradedReason)
	}
}

func TestRefreshUnrecoverable(t *testing.T) {
	s := baseStripe()
	dirs := allOnline()
	for _, i := range []int{0, 1, 2} {
		dirs.Dirs[i].Online = false
	}
	s.Refresh(dirs)
	if s.Status != StatusUnrecoverable || s.AvailableBlocks != 3 {
		t.Fatalf("unexpected: status=%s available=%d", s.Status, s.AvailableBlocks)
	}
}
