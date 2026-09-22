package main

import (
	"encoding/json"
	"fmt"
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

func TestPlanPadsIndex(t *testing.T) {
	mk := func(n int) []Record {
		rs := make([]Record, n)
		for i := range rs {
			rs[i] = Record{Identifier: fmt.Sprintf("r%d", i), ItemCategory: "c"}
		}
		return rs
	}
	rep := Report{Duplicates: []Group{{Title: "A", Records: mk(9)}, {Title: "B", Records: mk(10)}, {Title: "C", Records: mk(100)}}}
	data, _ := json.Marshal(rep)
	p := filepath.Join(t.TempDir(), "in.json")
	os.WriteFile(p, data, 0o644)
	jobs, err := plan(&Config{InputFile: p, ItemCategory: "c", Suffix: " s"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{0: "A s-1", 8: "A s-9", 9: "B s-01", 18: "B s-10", 19: "C s-001", 118: "C s-100"}
	for i, w := range want {
		if jobs[i].newTitle != w {
			t.Errorf("job %d = %q, want %q", i, jobs[i].newTitle, w)
		}
	}
}
