package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// CollectionIdentifier is substituted for {collection_identifier} in
	// RootPath. It can be overridden with the -collection_identifier flag.
	CollectionIdentifier string `yaml:"collection_identifier"`
	// RootPath is the backup's root directory, the one holding the
	// <identifier>/ directories (before flatten) or the files (after).
	RootPath string `yaml:"root_path"`
	// Subdir is the directory inside each <identifier>/ that holds the files.
	Subdir string `yaml:"subdir"`
	// Extensions lists the file extensions (case-insensitive) that are moved.
	Extensions []string `yaml:"extensions"`
	// Concurrency is the number of goroutines moving files in parallel.
	Concurrency int `yaml:"concurrency"`
	// ManifestFile, written into RootPath by flatten, records each moved
	// file's original location so expand can put it back exactly.
	ManifestFile string `yaml:"manifest_file"`
	// IdentifierPattern is used by expand for files missing from the
	// manifest: it is matched against the file name without its extension,
	// and its first capture group is the <identifier> directory. When empty,
	// the whole file name without its extension is the identifier.
	IdentifierPattern string `yaml:"identifier_pattern"`
}

// Manifest maps a file name in the flattened root to its path, relative to
// the root, before flatten moved it.
type Manifest map[string]string

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.RootPath == "" {
		return nil, errors.New("root_path is required")
	}
	if c.Subdir == "" {
		c.Subdir = "Access"
	}
	if len(c.Extensions) == 0 {
		c.Extensions = []string{".tif"}
	}
	if c.Concurrency < 1 {
		c.Concurrency = 16
	}
	if c.ManifestFile == "" {
		c.ManifestFile = "flatten_manifest.json"
	}
	return &c, nil
}

// resolveRoot fills {collection_identifier} into root_path.
func (c *Config) resolveRoot() (string, error) {
	if strings.Contains(c.RootPath, "{collection_identifier}") && c.CollectionIdentifier == "" {
		return "", errors.New("collection_identifier is required (config or -collection_identifier)")
	}
	return expandHome(strings.ReplaceAll(c.RootPath, "{collection_identifier}", c.CollectionIdentifier)), nil
}

func (c *Config) matchesExt(name string) bool {
	ext := filepath.Ext(name)
	for _, e := range c.Extensions {
		if strings.EqualFold(ext, e) {
			return true
		}
	}
	return false
}

func readManifest(path string) (Manifest, error) {
	m := Manifest{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// writeManifest writes m to path, or removes path when m is empty.
func writeManifest(path string, m Manifest) error {
	if len(m) == 0 {
		err := os.Remove(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Move is one file relocation, with paths relative to the root.
type Move struct {
	From string
	To   string
}

type Result struct {
	Moved   []Move
	Removed []string // directories removed (flatten only)
	Skipped []string // human-readable reasons something was left alone
	Errors  []error
}

// claims guards against two goroutines moving different files onto the same
// destination, which os.Rename would silently allow.
type claims struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (c *claims) claim(p string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[p] {
		return false
	}
	c.seen[p] = true
	return true
}

// moveFile renames root/from to root/to without overwriting. When dryRun is
// set, it only checks that the move would succeed.
func moveFile(root string, mv Move, cl *claims, dryRun bool) error {
	dst := filepath.Join(root, mv.To)
	if !cl.claim(dst) {
		return fmt.Errorf("%s: destination %s claimed by another file", mv.From, mv.To)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s: destination %s already exists", mv.From, mv.To)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(filepath.Join(root, mv.From), dst)
}

// collector gathers results from the worker goroutines.
type collector struct {
	mu  sync.Mutex
	res Result
}

func (c *collector) moved(mv Move) {
	c.mu.Lock()
	c.res.Moved = append(c.res.Moved, mv)
	c.mu.Unlock()
}

func (c *collector) removed(dir string) {
	c.mu.Lock()
	c.res.Removed = append(c.res.Removed, dir)
	c.mu.Unlock()
}

func (c *collector) skip(format string, a ...any) {
	c.mu.Lock()
	c.res.Skipped = append(c.res.Skipped, fmt.Sprintf(format, a...))
	c.mu.Unlock()
}

func (c *collector) fail(err error) {
	c.mu.Lock()
	c.res.Errors = append(c.res.Errors, err)
	c.mu.Unlock()
}

func (c *collector) result() Result {
	sort.Slice(c.res.Moved, func(i, j int) bool { return c.res.Moved[i].From < c.res.Moved[j].From })
	sort.Strings(c.res.Removed)
	sort.Strings(c.res.Skipped)
	return c.res
}

// runPool calls work for each item on cfg.Concurrency goroutines.
func runPool[T any](concurrency int, items []T, work func(T)) {
	ch := make(chan T)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				work(it)
			}
		}()
	}
	for _, it := range items {
		ch <- it
	}
	close(ch)
	wg.Wait()
}

// Flatten moves root/<identifier>/<subdir>/<file> to root/<file> for every
// matching file, then removes <subdir> and <identifier> if they are left
// empty. Top-level directories without a <subdir> are left alone. Each
// moved file is added to the manifest.
func Flatten(cfg *Config, root string, dryRun bool) (Result, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Result{}, err
	}
	manifestPath := filepath.Join(root, cfg.ManifestFile)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return Result{}, err
	}

	var col collector
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, err := os.Stat(filepath.Join(root, e.Name(), cfg.Subdir)); err != nil || !fi.IsDir() {
			col.skip("%s/: no %s/ directory", e.Name(), cfg.Subdir)
			continue
		}
		dirs = append(dirs, e.Name())
	}

	cl := &claims{seen: map[string]bool{}}
	runPool(cfg.Concurrency, dirs, func(id string) {
		sub := filepath.Join(id, cfg.Subdir)
		files, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			col.fail(err)
			return
		}
		for _, f := range files {
			rel := filepath.Join(sub, f.Name())
			if !f.Type().IsRegular() || !cfg.matchesExt(f.Name()) {
				col.skip("%s: not a matching file", rel)
				continue
			}
			mv := Move{From: rel, To: f.Name()}
			if err := moveFile(root, mv, cl, dryRun); err != nil {
				col.fail(err)
				continue
			}
			col.moved(mv)
		}
		if dryRun {
			return
		}
		for _, d := range []string{sub, id} {
			if err := os.Remove(filepath.Join(root, d)); err != nil {
				col.skip("%s/: not removed, not empty", d)
				return
			}
			col.removed(d)
		}
	})

	res := col.result()
	if !dryRun && len(res.Moved) > 0 {
		for _, mv := range res.Moved {
			manifest[mv.To] = mv.From
		}
		if err := writeManifest(manifestPath, manifest); err != nil {
			return res, fmt.Errorf("write manifest: %w", err)
		}
	}
	return res, nil
}

// Expand is the reverse of Flatten: it moves each matching file in root back
// to the location recorded in the manifest or, for files not in the
// manifest, to root/<identifier>/<subdir>/<file> with <identifier> derived
// from cfg.IdentifierPattern.
func Expand(cfg *Config, root string, dryRun bool) (Result, error) {
	var pattern *regexp.Regexp
	if cfg.IdentifierPattern != "" {
		var err error
		if pattern, err = regexp.Compile(cfg.IdentifierPattern); err != nil {
			return Result{}, fmt.Errorf("identifier_pattern: %w", err)
		}
		if pattern.NumSubexp() < 1 {
			return Result{}, errors.New("identifier_pattern: needs a capture group for the identifier")
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return Result{}, err
	}
	manifestPath := filepath.Join(root, cfg.ManifestFile)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return Result{}, err
	}

	var col collector
	var moves []Move
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !cfg.matchesExt(name) {
			continue
		}
		if to, ok := manifest[name]; ok {
			moves = append(moves, Move{From: name, To: to})
			continue
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if pattern != nil {
			m := pattern.FindStringSubmatch(id)
			if m == nil || m[1] == "" {
				col.skip("%s: not in manifest and does not match identifier_pattern", name)
				continue
			}
			id = m[1]
		}
		moves = append(moves, Move{From: name, To: filepath.Join(id, cfg.Subdir, name)})
	}

	cl := &claims{seen: map[string]bool{}}
	runPool(cfg.Concurrency, moves, func(mv Move) {
		if err := moveFile(root, mv, cl, dryRun); err != nil {
			col.fail(err)
			return
		}
		col.moved(mv)
	})

	res := col.result()
	if !dryRun && len(res.Moved) > 0 {
		for _, mv := range res.Moved {
			delete(manifest, mv.From)
		}
		if err := writeManifest(manifestPath, manifest); err != nil {
			return res, fmt.Errorf("write manifest: %w", err)
		}
	}
	return res, nil
}

func printResult(cmd string, res Result, dryRun, verbose bool) {
	verb := "moved"
	if dryRun {
		verb = "would move"
	}
	if verbose {
		for _, mv := range res.Moved {
			fmt.Printf("%s: %s -> %s\n", verb, mv.From, mv.To)
		}
		for _, d := range res.Removed {
			fmt.Printf("removed: %s/\n", d)
		}
	}
	for _, s := range res.Skipped {
		fmt.Println("skipped:", s)
	}
	for _, err := range res.Errors {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	fmt.Printf("%s: %s %d files", cmd, verb, len(res.Moved))
	if cmd == "flatten" && !dryRun {
		fmt.Printf(", removed %d directories", len(res.Removed))
	}
	fmt.Printf(", skipped %d, errors %d\n", len(res.Skipped), len(res.Errors))
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: flatten-collection <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  flatten  move <identifier>/<subdir>/<file> up to the root and remove the emptied directories")
	fmt.Fprintln(os.Stderr, "  expand   move root files back into <identifier>/<subdir>/ (reverse of flatten)")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	var run func(*Config, string, bool) (Result, error)
	switch cmd {
	case "flatten":
		run = Flatten
	case "expand":
		run = Expand
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "flatten-collection: unknown command %q\n", cmd)
		usage()
	}

	fset := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fset.String("config", "config.yaml", "path to YAML config")
	collID := fset.String("collection_identifier", "", "collection identifier (overrides config)")
	dryRun := fset.Bool("dry_run", false, "report what would be moved without changing anything")
	verbose := fset.Bool("v", false, "print every file moved and directory removed")
	fset.Parse(os.Args[2:])

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if *collID != "" {
		cfg.CollectionIdentifier = *collID
	}
	root, err := cfg.resolveRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	fmt.Println("root:", root)

	res, err := run(cfg, root, *dryRun)
	printResult(cmd, res, *dryRun, *verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, cmd+":", err)
		os.Exit(1)
	}
	if len(res.Errors) > 0 {
		os.Exit(1)
	}
}
