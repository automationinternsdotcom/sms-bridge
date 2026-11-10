package store

import (
	"path/filepath"
	"testing"
)

func TestRepairDoesNotDuplicate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Message{PairingID: "pairA", MessageID: "100", ConvID: "c1", TsUS: 1_700_000_000_000_000, Direction: "in", Body: "hi", Status: 100}
	if n, _ := s.Upsert(m); !n {
		t.Fatal("first insert should be new")
	}
	m2 := *m
	m2.PairingID = "pairB" // re-paired
	if n, _ := s.Upsert(&m2); n {
		t.Fatal("same message under new pairing must not be new")
	}
	if !s.Known("c1", "100") {
		t.Fatal("Known should match across pairings")
	}
	m3 := *m
	m3.TsUS += 3600_000_000 // ID reused for a different message
	if n, _ := s.Upsert(&m3); !n {
		t.Fatal("reused ID with different timestamp should be a new row")
	}
	m4 := *m
	m4.Status = 109 // status update
	if n, _ := s.Upsert(&m4); n {
		t.Fatal("status update should not be new")
	}
	var cnt int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&cnt)
	if cnt != 2 {
		t.Fatalf("want 2 rows, got %d", cnt)
	}
}
