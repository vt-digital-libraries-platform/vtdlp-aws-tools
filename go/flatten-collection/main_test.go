package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func testConfig() *Config {
	return &Config{
		Subdir:            "Access",
		Extensions:        []string{".tif"},
		Concurrency:       4,
		ManifestFile:      "flatten_manifest.json",
		IdentifierPattern: `^(.+)\d{3}$`,
	}
}

func writeFiles(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// listFiles returns every regular file under root, relative to root.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestFlattenThenExpand_RoundTrips(t *testing.T) {
	root := t.TempDir()
	original := []string{
		"a001/Access/a001001.tif",
		"a001/Access/a001002.TIF",
		"b002/Access/b002001.tif",
		"notes.csv",
		"targets/t1.tif",
	}
	writeFiles(t, root, original...)
	cfg := testConfig()

	res, err := Flatten(cfg, root, false)
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("flatten: %v %v", err, res.Errors)
	}
	want := []string{"a001001.tif", "a001002.TIF", "b002001.tif", "flatten_manifest.json", "notes.csv", "targets/t1.tif"}
	if got := listFiles(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("after flatten got %v, want %v", got, want)
	}
	for _, d := range []string{"a001", "b002"} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", d)
		}
	}

	res, err = Expand(cfg, root, false)
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("expand: %v %v", err, res.Errors)
	}
	if got := listFiles(t, root); !reflect.DeepEqual(got, original) {
		t.Fatalf("after expand got %v, want %v", got, original)
	}
}

func TestFlatten_KeepsNonEmptyDirsAndRefusesCollisions(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root,
		"a001/Access/a001001.tif",
		"a001/Access/readme.txt",
		"b002/Access/a001001.tif", // same name as a001's file
		"c003/Access/c003001.tif",
		"c003001.tif", // already at the root
	)

	res, err := Flatten(testConfig(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 1 || len(res.Errors) != 2 {
		t.Fatalf("moved %v, errors %v; want 1 move and 2 collisions", res.Moved, res.Errors)
	}
	for _, p := range []string{"a001/Access/readme.txt", "c003/Access/c003001.tif"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s should be untouched: %v", p, err)
		}
	}
}

func TestExpand_UsesIdentifierPatternWithoutManifest(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "x123001.tif", "x123002.tif", "ab.tif", "other.csv")

	res, err := Expand(testConfig(), root, false)
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("expand: %v %v", err, res.Errors)
	}
	want := []string{"ab.tif", "other.csv", "x123/Access/x123001.tif", "x123/Access/x123002.tif"}
	if got := listFiles(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("skipped %v; want ab.tif skipped", res.Skipped)
	}
}

func TestDryRun_ChangesNothing(t *testing.T) {
	root := t.TempDir()
	files := []string{"a001/Access/a001001.tif"}
	writeFiles(t, root, files...)

	res, err := Flatten(testConfig(), root, true)
	if err != nil || len(res.Moved) != 1 {
		t.Fatalf("flatten dry run: %v moved %v", err, res.Moved)
	}
	if got := listFiles(t, root); !reflect.DeepEqual(got, files) {
		t.Fatalf("dry run changed files: %v", got)
	}
}

func TestResolveRoot(t *testing.T) {
	cfg := &Config{RootPath: "/data/{collection_identifier}/media", CollectionIdentifier: "glink"}
	if got, _ := cfg.resolveRoot(); got != "/data/glink/media" {
		t.Errorf("got %q", got)
	}
	cfg.CollectionIdentifier = ""
	if _, err := cfg.resolveRoot(); err == nil {
		t.Error("want error when collection_identifier is missing")
	}
}
