package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Region    string `yaml:"region"`
	TableName string `yaml:"table_name"`
	// CollectionTableName is used by `report` to resolve each record's
	// collection id to the collection's identifier. Required for `report`.
	CollectionTableName string `yaml:"collection_table_name"`
	OutputDir           string `yaml:"output_dir"`
	OutputFile          string `yaml:"output_file"`
	// Concurrency is the number of parallel scan segments (one goroutine each),
	// and also the number of parallel writers used by apply/rollback.
	Concurrency int `yaml:"concurrency"`
	// CollectionIdentifier and Suffix are fallback defaults for apply's
	// -collection_identifier/-suffix flags, used only when a flag is omitted.
	CollectionIdentifier string `yaml:"collection_identifier"`
	Suffix               string `yaml:"suffix"`
}

// Report is the shape written by `report` and read back by `apply` and
// `rollback`. `apply` also writes its change-log in this same shape (with
// CollectionIdentifier/Timestamp populated and each written record's
// NewTitle filled in) so that `rollback` can accept either file
// interchangeably.
type Report struct {
	CollectionIdentifier string  `json:"collection_identifier,omitempty"`
	Timestamp            string  `json:"timestamp,omitempty"`
	Duplicates           []Group `json:"duplicates"`
}

type Group struct {
	Title   string   `json:"title"`
	Records []Record `json:"records"`
}

type Record struct {
	Id         string `json:"id"`
	Identifier string `json:"identifier"`
	// CollectionID is the record's parent collection's id (DynamoDB key into
	// the collection table), taken from the Archive item's parent_collection
	// attribute.
	CollectionID *string `json:"collection_id"`
	// CollectionIdentifier is the parent collection's human-readable
	// identifier, resolved from CollectionID via the collection table.
	CollectionIdentifier *string `json:"collection_identifier"`
	// NewTitle is set only in a change-log written by `apply`, recording the
	// title that was actually written for this record.
	NewTitle *string `json:"new_title,omitempty"`
}

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
	if c.TableName == "" {
		return nil, errors.New("table_name is required")
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.OutputDir == "" {
		c.OutputDir = "output"
	}
	if c.OutputFile == "" {
		c.OutputFile = "duplicate_titles.json"
	}
	if c.Concurrency < 1 {
		c.Concurrency = 10
	}
	c.OutputDir = expandHome(c.OutputDir)
	return &c, nil
}

func str(it map[string]types.AttributeValue, name string) (string, bool) {
	v, ok := it[name].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

// buildCollectionIndex scans the collection table and returns a map of
// collection id -> collection identifier, used to resolve each record's
// CollectionID to a CollectionIdentifier.
func buildCollectionIndex(ctx context.Context, db *dynamodb.Client, table string) (map[string]string, error) {
	index := map[string]string{}
	p := dynamodb.NewScanPaginator(db, &dynamodb.ScanInput{
		TableName:                aws.String(table),
		ProjectionExpression:     aws.String("#k, #i"),
		ExpressionAttributeNames: map[string]string{"#k": "id", "#i": "identifier"},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			id, ok := str(it, "id")
			if !ok {
				continue
			}
			if ident, ok := str(it, "identifier"); ok {
				index[id] = ident
			}
		}
	}
	return index, nil
}

// firstParent returns parent_collection[0], or nil if the attribute is
// missing or empty.
func firstParent(it map[string]types.AttributeValue) *string {
	switch v := it["parent_collection"].(type) {
	case *types.AttributeValueMemberL:
		if len(v.Value) > 0 {
			if s, ok := v.Value[0].(*types.AttributeValueMemberS); ok {
				return &s.Value
			}
		}
	case *types.AttributeValueMemberSS:
		if len(v.Value) > 0 {
			return &v.Value[0]
		}
	}
	return nil
}

// scanSegment scans one segment of the table into a title -> records map.
// collIndex resolves each record's collection id to its collection
// identifier (see buildCollectionIndex).
func scanSegment(ctx context.Context, db *dynamodb.Client, table string, seg, total int, collIndex map[string]string) (map[string][]Record, int, error) {
	byTitle := map[string][]Record{}
	scanned := 0
	in := &dynamodb.ScanInput{
		TableName:            aws.String(table),
		ProjectionExpression: aws.String("#t, #i, #p, #id"),
		ExpressionAttributeNames: map[string]string{
			"#t": "title", "#i": "identifier", "#p": "parent_collection", "#id": "id",
		},
	}
	if total > 1 {
		in.Segment = aws.Int32(int32(seg))
		in.TotalSegments = aws.Int32(int32(total))
	}
	p := dynamodb.NewScanPaginator(db, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, 0, err
		}
		for _, it := range page.Items {
			scanned++
			title, ok := str(it, "title")
			if !ok || title == "" {
				continue
			}
			ident, _ := str(it, "identifier")
			id, _ := str(it, "id")
			collID := firstParent(it)
			var collIdent *string
			if collID != nil {
				if v, ok := collIndex[*collID]; ok {
					collIdent = &v
				}
			}
			byTitle[title] = append(byTitle[title], Record{Id: id, Identifier: ident, CollectionID: collID, CollectionIdentifier: collIdent})
		}
	}
	return byTitle, scanned, nil
}

// findDuplicates scans the table with one goroutine per segment and groups
// records by exact title, keeping only titles that occur more than once.
// collIndex resolves each record's collection id to its collection
// identifier (see buildCollectionIndex).
func findDuplicates(ctx context.Context, db *dynamodb.Client, table string, workers int, collIndex map[string]string) (*Report, int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		scanned  int
		byTitle  = map[string][]Record{}
	)
	for seg := 0; seg < workers; seg++ {
		wg.Add(1)
		go func(seg int) {
			defer wg.Done()
			part, n, err := scanSegment(ctx, db, table, seg, workers, collIndex)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("segment %d: %w", seg, err)
					cancel()
				}
				return
			}
			scanned += n
			for t, recs := range part {
				byTitle[t] = append(byTitle[t], recs...)
			}
		}(seg)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, 0, firstErr
	}

	rep := &Report{Duplicates: []Group{}}
	for title, recs := range byTitle {
		if len(recs) < 2 {
			continue
		}
		sort.Slice(recs, func(a, b int) bool { return recs[a].Identifier < recs[b].Identifier })
		rep.Duplicates = append(rep.Duplicates, Group{Title: title, Records: recs})
	}
	sort.Slice(rep.Duplicates, func(a, b int) bool { return rep.Duplicates[a].Title < rep.Duplicates[b].Title })
	return rep, scanned, nil
}

// loadReport reads a report or change-log file (same JSON shape) from path.
func loadReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// Job is a single planned write: set the record identified by Id's title to
// NewTitle. OldTitle, CollectionID and CollectionIdentifier are carried
// along purely for logging/audit and for writing the change-log.
type Job struct {
	Id                   string
	Identifier           string
	CollectionID         string
	CollectionIdentifier string
	OldTitle             string
	NewTitle             string
}

// planApply computes the writes needed to disambiguate every record in rep
// belonging to collectionIdentifier: <original title> - <suffix>:<identifier>.
// Records in other collections are left untouched, including other members
// of a title-group that spans multiple collections.
func planApply(rep *Report, collectionIdentifier, suffix string) []Job {
	var jobs []Job
	for _, g := range rep.Duplicates {
		for _, r := range g.Records {
			if r.CollectionIdentifier == nil || *r.CollectionIdentifier != collectionIdentifier {
				continue
			}
			var collID string
			if r.CollectionID != nil {
				collID = *r.CollectionID
			}
			jobs = append(jobs, Job{
				Id:                   r.Id,
				Identifier:           r.Identifier,
				CollectionID:         collID,
				CollectionIdentifier: *r.CollectionIdentifier,
				OldTitle:             g.Title,
				NewTitle:             fmt.Sprintf("%s - %s: %s", g.Title, suffix, r.Identifier),
			})
		}
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Identifier < jobs[b].Identifier })
	return jobs
}

// planRollback computes the writes needed to restore every record in rep
// belonging to collectionIdentifier to its recorded title (g.Title),
// regardless of the record's current live value in DynamoDB. Records in
// other collections are left untouched, including other members of a
// title-group that spans multiple collections. rep may be an original
// report or a change-log written by apply; both share this shape.
func planRollback(rep *Report, collectionIdentifier string) []Job {
	var jobs []Job
	for _, g := range rep.Duplicates {
		for _, r := range g.Records {
			if r.CollectionIdentifier == nil || *r.CollectionIdentifier != collectionIdentifier {
				continue
			}
			old := ""
			if r.NewTitle != nil {
				old = *r.NewTitle
			}
			jobs = append(jobs, Job{
				Id:         r.Id,
				Identifier: r.Identifier,
				OldTitle:   old,
				NewTitle:   g.Title,
			})
		}
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Identifier < jobs[b].Identifier })
	return jobs
}

// writeJobs sets title = job.NewTitle for each job, using up to concurrency
// goroutines, and returns the number of records successfully written.
func writeJobs(ctx context.Context, db *dynamodb.Client, table string, jobs []Job, concurrency int) (int, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		written  int
		sem      = make(chan struct{}, concurrency)
	)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j Job) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName: aws.String(table),
				Key: map[string]types.AttributeValue{
					"id": &types.AttributeValueMemberS{Value: j.Id},
				},
				UpdateExpression: aws.String("SET #t = :t"),
				ExpressionAttributeNames: map[string]string{
					"#t": "title",
				},
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":t": &types.AttributeValueMemberS{Value: j.NewTitle},
				},
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("update %s (id=%s): %w", j.Identifier, j.Id, err)
				}
				return
			}
			written++
		}(j)
	}
	wg.Wait()
	return written, firstErr
}

// buildChangeLog groups the jobs actually written into the same shape as a
// report, so that `rollback` can later read this file back with the exact
// same code path used for an original report.
func buildChangeLog(collectionIdentifier string, jobs []Job) *Report {
	byTitle := map[string][]Record{}
	var titles []string
	for _, j := range jobs {
		newTitle := j.NewTitle
		if _, ok := byTitle[j.OldTitle]; !ok {
			titles = append(titles, j.OldTitle)
		}
		collID := j.CollectionID
		collIdent := j.CollectionIdentifier
		byTitle[j.OldTitle] = append(byTitle[j.OldTitle], Record{
			Id:                   j.Id,
			Identifier:           j.Identifier,
			CollectionID:         &collID,
			CollectionIdentifier: &collIdent,
			NewTitle:             &newTitle,
		})
	}
	sort.Strings(titles)
	rep := &Report{
		CollectionIdentifier: collectionIdentifier,
		Timestamp:            time.Now().UTC().Format("20060102T150405Z"),
		Duplicates:           []Group{},
	}
	for _, t := range titles {
		rep.Duplicates = append(rep.Duplicates, Group{Title: t, Records: byTitle[t]})
	}
	return rep
}

// usage lists the available commands and exits 2, matching the flag
// package's own convention for a bad invocation.
func usage() {
	fmt.Fprintln(os.Stderr, "usage: archive-title-dedup <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  report    scan the table and write the duplicate-titles report")
	fmt.Fprintln(os.Stderr, "  apply     disambiguate one collection's duplicate titles")
	fmt.Fprintln(os.Stderr, "  rollback  restore titles recorded in a report or change-log file")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch cmd := os.Args[1]; cmd {
	case "report":
		runReport(os.Args[2:])
	case "apply":
		runApply(os.Args[2:])
	case "rollback":
		runRollback(os.Args[2:])
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "archive-title-dedup: unknown command %q\n", cmd)
		usage()
	}
}

func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config")
	fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if cfg.CollectionTableName == "" {
		fmt.Fprintln(os.Stderr, "config: collection_table_name is required")
		os.Exit(1)
	}

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws:", err)
		os.Exit(1)
	}
	db := dynamodb.NewFromConfig(awsCfg)

	collIndex, err := buildCollectionIndex(ctx, db, cfg.CollectionTableName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan collection table:", err)
		os.Exit(1)
	}

	rep, scanned, err := findDuplicates(ctx, db, cfg.TableName, cfg.Concurrency, collIndex)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "output:", err)
		os.Exit(1)
	}
	out := filepath.Join(cfg.OutputDir, cfg.OutputFile)
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "output:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "output:", err)
		os.Exit(1)
	}
	fmt.Printf("scanned %d records; %d duplicate titles written to %s\n", scanned, len(rep.Duplicates), out)
}

func runApply(args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config")
	inputPath := fs.String("input", "", "path to the input report or change-log JSON (required)")
	collectionIdentifier := fs.String("collection_identifier", "", "collection to disambiguate (falls back to config.yaml)")
	suffix := fs.String("suffix", "", "human-readable label inserted before the identifier (falls back to config.yaml)")
	dryRun := fs.Bool("dry-run", false, "log planned changes without writing to DynamoDB")
	fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	if *inputPath == "" {
		fmt.Fprintln(os.Stderr, "apply: -input is required")
		os.Exit(2)
	}
	collID := *collectionIdentifier
	if collID == "" {
		collID = cfg.CollectionIdentifier
	}
	if collID == "" {
		fmt.Fprintln(os.Stderr, "apply: -collection_identifier is required (flag or config.yaml)")
		os.Exit(2)
	}
	suf := *suffix
	if suf == "" {
		suf = cfg.Suffix
	}
	if suf == "" {
		fmt.Fprintln(os.Stderr, "apply: -suffix is required (flag or config.yaml)")
		os.Exit(2)
	}

	rep, err := loadReport(*inputPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "input:", err)
		os.Exit(1)
	}

	jobs := planApply(rep, collID, suf)
	if len(jobs) == 0 {
		fmt.Printf("no records found for collection %s in %s\n", collID, *inputPath)
		return
	}

	for _, j := range jobs {
		fmt.Printf("%s: %q -> %q\n", j.Identifier, j.OldTitle, j.NewTitle)
	}
	if *dryRun {
		fmt.Printf("dry-run: %d records would be updated in collection %s\n", len(jobs), collID)
		return
	}

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws:", err)
		os.Exit(1)
	}
	db := dynamodb.NewFromConfig(awsCfg)

	written, err := writeJobs(ctx, db, cfg.TableName, jobs, cfg.Concurrency)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
	}

	changeLog := buildChangeLog(collID, jobs)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "output:", err)
		os.Exit(1)
	}
	logPath := filepath.Join(cfg.OutputDir, fmt.Sprintf("changelog_%s_%s.json", collID, changeLog.Timestamp))
	data, mErr := json.MarshalIndent(changeLog, "", "  ")
	if mErr != nil {
		fmt.Fprintln(os.Stderr, "output:", mErr)
		os.Exit(1)
	}
	if wErr := os.WriteFile(logPath, append(data, '\n'), 0o644); wErr != nil {
		fmt.Fprintln(os.Stderr, "output:", wErr)
		os.Exit(1)
	}
	fmt.Printf("wrote %d/%d records; change-log written to %s\n", written, len(jobs), logPath)
	if err != nil {
		os.Exit(1)
	}
}

func runRollback(args []string) {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config")
	collectionIdentifier := fs.String("collection_identifier", "", "collection to revert (falls back to config.yaml)")
	dryRun := fs.Bool("dry-run", false, "log planned changes without writing to DynamoDB")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: archive-title-dedup rollback [-config config.yaml] [-collection_identifier id] [-dry-run] <report-or-changelog.json>")
		os.Exit(2)
	}
	inputPath := fs.Arg(0)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	collID := *collectionIdentifier
	if collID == "" {
		collID = cfg.CollectionIdentifier
	}
	if collID == "" {
		fmt.Fprintln(os.Stderr, "rollback: -collection_identifier is required (flag or config.yaml)")
		os.Exit(2)
	}

	rep, err := loadReport(inputPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "input:", err)
		os.Exit(1)
	}

	jobs := planRollback(rep, collID)
	if len(jobs) == 0 {
		fmt.Printf("no records found for collection %s in %s\n", collID, inputPath)
		return
	}

	for _, j := range jobs {
		fmt.Printf("%s: %q -> %q\n", j.Identifier, j.OldTitle, j.NewTitle)
	}
	if *dryRun {
		fmt.Printf("dry-run: %d records would be reverted\n", len(jobs))
		return
	}

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws:", err)
		os.Exit(1)
	}
	db := dynamodb.NewFromConfig(awsCfg)

	written, err := writeJobs(ctx, db, cfg.TableName, jobs, cfg.Concurrency)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
	}
	fmt.Printf("reverted %d/%d records\n", written, len(jobs))
	if err != nil {
		os.Exit(1)
	}
}
