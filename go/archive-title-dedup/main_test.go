package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadChangesCollapsesAndChecksTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.jsonl")
	log := `{"id":"a","identifier":"A","table":"T","old_title":"x","new_title":"x-1"}
{"id":"b","identifier":"B","table":"T","old_title":"y","new_title":"y-1"}

{"id":"a","identifier":"A","table":"T","old_title":"x-1","new_title":"x-2"}
`
	if err := os.WriteFile(p, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readChanges(p, "T")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[0].OldTitle != "x" || got[0].NewTitle != "x-2" || got[1].ID != "b" {
		t.Fatalf("unexpected changes: %+v", got)
	}
	if _, err := readChanges(p, "other"); err == nil {
		t.Fatal("expected table mismatch error")
	}
}
