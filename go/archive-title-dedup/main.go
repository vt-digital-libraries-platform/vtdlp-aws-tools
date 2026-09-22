package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Region       string `yaml:"region"`
	TableName    string `yaml:"table_name"`
	InputFile    string `yaml:"input_file"`
	ItemCategory string `yaml:"item_category"`
	// CollectionIdentifier, if set, restricts changes to records whose
	// parent_collection_identifier in the report equals it.
	CollectionIdentifier string `yaml:"collection_identifier"`
	// CollectionTableName is used to resolve collection_identifier to its
	// Collection id so Archive queries can be scoped to it. Required when
	// collection_identifier is set.
	CollectionTableName string `yaml:"collection_table_name"`
	Suffix              string `yaml:"suffix"`
	Concurrency         int    `yaml:"concurrency"`
	DryRun              bool   `yaml:"dry_run"`
	// ChangeLog is a JSON-lines file: every applied title change is appended
	// to it, and rollback reads it to restore the original titles.
	ChangeLog string `yaml:"change_log"`
	// Rollback restores titles from ChangeLog instead of applying changes.
	// Also enabled by the -rollback flag.
	Rollback bool `yaml:"rollback"`
}

// change is one line of the change log.
type change struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Table      string `json:"table"`
	OldTitle   string `json:"old_title"`
	NewTitle   string `json:"new_title"`
}

type Report struct {
	Duplicates []Group `json:"duplicates"`
}

type Group struct {
	Title   string   `json:"title"`
	Records []Record `json:"records"`
}

type Record struct {
	Identifier                 string  `json:"identifier"`
	CollectionID               string  `json:"collection_id"`
	ItemCategory               string  `json:"item_category"`
	ParentCollectionIdentifier *string `json:"parent_collection_identifier"`
}

type job struct {
	identifier string
	oldTitle   string
	newTitle   string
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
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.Concurrency < 1 {
		c.Concurrency = 10
	}
	if c.ChangeLog == "" {
		c.ChangeLog = "title-changes.jsonl"
	}
	c.InputFile = expandHome(c.InputFile)
	c.ChangeLog = expandHome(c.ChangeLog)
	return &c, nil
}

// validate checks the settings needed for the selected mode.
func (c *Config) validate() error {
	if c.TableName == "" {
		return errors.New("table_name is required")
	}
	if c.Rollback {
		return nil
	}
	return c.validateApply()
}

// validateApply checks the settings needed to compute changes from the input
// report (used by a normal run and by rollback without a change log).
func (c *Config) validateApply() error {
	switch {
	case c.InputFile == "":
		return errors.New("input_file is required")
	case c.ItemCategory == "":
		return errors.New("item_category is required")
	case c.Suffix == "":
		return errors.New("suffix is required")
	case c.CollectionIdentifier != "" && c.CollectionTableName == "":
		return errors.New("collection_table_name is required when collection_identifier is set")
	}
	return nil
}

// plan builds the list of title changes: every record in a duplicate group
// whose item_category (and, if configured, parent_collection_identifier)
// matches gets "<title><suffix>-<n>", n starting at 1 within the group and
// zero-padded so titles sort.
func plan(cfg *Config) ([]job, error) {
	data, err := os.ReadFile(cfg.InputFile)
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	var jobs []job
	for _, g := range rep.Duplicates {
		var matched []Record
		for _, r := range g.Records {
			if r.ItemCategory != cfg.ItemCategory {
				continue
			}
			if cfg.CollectionIdentifier != "" &&
				(r.ParentCollectionIdentifier == nil || *r.ParentCollectionIdentifier != cfg.CollectionIdentifier) {
				continue
			}
			matched = append(matched, r)
		}
		// Zero-pad the index to the width of the largest index in the group
		// (10-99 records -> -01, 100-999 -> -001, ...) so titles sort correctly.
		width := len(strconv.Itoa(len(matched)))
		for i, r := range matched {
			jobs = append(jobs, job{
				identifier: r.Identifier,
				oldTitle:   g.Title,
				newTitle:   fmt.Sprintf("%s%s-%0*d", g.Title, cfg.Suffix, width, i+1),
			})
		}
	}
	return jobs, nil
}

// collectionIDs resolves collection_identifier to the Collection row id(s).
func collectionIDs(ctx context.Context, db *dynamodb.Client, cfg *Config) ([]string, error) {
	var ids []string
	p := dynamodb.NewScanPaginator(db, &dynamodb.ScanInput{
		TableName:                aws.String(cfg.CollectionTableName),
		FilterExpression:         aws.String("#i = :i"),
		ProjectionExpression:     aws.String("#k"),
		ExpressionAttributeNames: map[string]string{"#i": "identifier", "#k": "id"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":i": &types.AttributeValueMemberS{Value: cfg.CollectionIdentifier},
		},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if v, ok := it["id"].(*types.AttributeValueMemberS); ok {
				ids = append(ids, v.Value)
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no collection with identifier %q in %s", cfg.CollectionIdentifier, cfg.CollectionTableName)
	}
	return ids, nil
}

// idsByIdentifier scans the Archive table and maps identifier -> id(s), since
// the table is keyed on id while the report only carries identifiers. When
// collection_identifier is set the scan is filtered to that collection only:
// records whose parent_collection contains it, or, for records with no
// parent_collection, whose collection is it. Nothing outside it is read.
func idsByIdentifier(ctx context.Context, db *dynamodb.Client, cfg *Config) (map[string][]string, error) {
	in := &dynamodb.ScanInput{
		TableName:                aws.String(cfg.TableName),
		ProjectionExpression:     aws.String("#i, #k"),
		ExpressionAttributeNames: map[string]string{"#i": "identifier", "#k": "id"},
	}
	if cfg.CollectionIdentifier != "" {
		cids, err := collectionIDs(ctx, db, cfg)
		if err != nil {
			return nil, err
		}
		in.ExpressionAttributeNames["#c"] = "collection"
		in.ExpressionAttributeNames["#p"] = "parent_collection"
		in.ExpressionAttributeValues = map[string]types.AttributeValue{
			":zero": &types.AttributeValueMemberN{Value: "0"},
		}
		var clauses []string
		for n, id := range cids {
			ph := fmt.Sprintf(":c%d", n)
			in.ExpressionAttributeValues[ph] = &types.AttributeValueMemberS{Value: id}
			clauses = append(clauses, fmt.Sprintf("contains(#p, %s) OR ((attribute_not_exists(#p) OR size(#p) = :zero) AND #c = %s)", ph, ph))
		}
		in.FilterExpression = aws.String(strings.Join(clauses, " OR "))
	}
	out := map[string][]string{}
	p := dynamodb.NewScanPaginator(db, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			idv, ok1 := it["id"].(*types.AttributeValueMemberS)
			iv, ok2 := it["identifier"].(*types.AttributeValueMemberS)
			if ok1 && ok2 {
				out[iv.Value] = append(out[iv.Value], idv.Value)
			}
		}
	}
	return out, nil
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to YAML config")
	dry := flag.Bool("dry-run", false, "report changes without writing")
	rollback := flag.Bool("rollback", false, "restore original titles from the change log")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if *dry {
		cfg.DryRun = true
	}
	if *rollback {
		cfg.Rollback = true
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws:", err)
		os.Exit(1)
	}
	db := dynamodb.NewFromConfig(awsCfg)

	if cfg.Rollback {
		if !runRollback(ctx, db, cfg) {
			os.Exit(1)
		}
		return
	}

	jobs, err := plan(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		os.Exit(1)
	}
	fmt.Printf("table=%s category=%q collection_identifier=%q planned=%d dry_run=%v\n", cfg.TableName, cfg.ItemCategory, cfg.CollectionIdentifier, len(jobs), cfg.DryRun)

	ids, err := idsByIdentifier(ctx, db, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}

	var logFile *os.File
	var enc *json.Encoder
	if !cfg.DryRun && len(jobs) > 0 {
		logFile, err = os.OpenFile(cfg.ChangeLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "change log:", err)
			os.Exit(1)
		}
		defer logFile.Close()
		enc = json.NewEncoder(logFile)
		enc.SetEscapeHTML(false)
	}

	var updated, skipped, failed atomic.Int64
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Printf(format+"\n", a...)
	}

	work := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				found := ids[j.identifier]
				if len(found) != 1 {
					logf("SKIP %s: %d table rows match identifier", j.identifier, len(found))
					skipped.Add(1)
					continue
				}
				if cfg.DryRun {
					logf("DRY  %s: %q -> %q", j.identifier, j.oldTitle, j.newTitle)
					updated.Add(1)
					continue
				}
				// Only write if the title and category are still what the
				// report says, so reruns and concurrent edits are safe.
				_, err := db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:           aws.String(cfg.TableName),
					Key:                 map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: found[0]}},
					UpdateExpression:    aws.String("SET #t = :new"),
					ConditionExpression: aws.String("#t = :old AND #c = :cat"),
					ExpressionAttributeNames: map[string]string{
						"#t": "title", "#c": "item_category",
					},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":new": &types.AttributeValueMemberS{Value: j.newTitle},
						":old": &types.AttributeValueMemberS{Value: j.oldTitle},
						":cat": &types.AttributeValueMemberS{Value: cfg.ItemCategory},
					},
				})
				var ccf *types.ConditionalCheckFailedException
				switch {
				case errors.As(err, &ccf):
					logf("SKIP %s: title/category changed since report", j.identifier)
					skipped.Add(1)
				case err != nil:
					logf("FAIL %s: %v", j.identifier, err)
					failed.Add(1)
				default:
					// Record the change before reporting it so a rollback
					// can always undo what was written.
					logMu.Lock()
					werr := enc.Encode(change{ID: found[0], Identifier: j.identifier, Table: cfg.TableName, OldTitle: j.oldTitle, NewTitle: j.newTitle})
					logMu.Unlock()
					if werr != nil {
						logf("WARN %s: change log write failed: %v", j.identifier, werr)
					}
					logf("OK   %s: %q -> %q", j.identifier, j.oldTitle, j.newTitle)
					updated.Add(1)
				}
			}
		}()
	}
	for _, j := range jobs {
		work <- j
	}
	close(work)
	wg.Wait()

	fmt.Printf("done: updated=%d skipped=%d failed=%d\n", updated.Load(), skipped.Load(), failed.Load())
	if !cfg.DryRun && updated.Load() > 0 {
		fmt.Printf("change log: %s (use -rollback to undo)\n", cfg.ChangeLog)
	}
	if failed.Load() > 0 {
		os.Exit(1)
	}
}

// readChanges loads the change log and collapses repeated changes to the same
// row into one: the earliest old title and the latest new title.
func readChanges(path, table string) ([]change, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byID := map[string]*change{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var c change
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if c.Table != table {
			return nil, fmt.Errorf("%s line %d: change is for table %s, config is %s", path, n, c.Table, table)
		}
		if prev, ok := byID[c.ID]; ok {
			prev.NewTitle = c.NewTitle
			continue
		}
		byID[c.ID] = &c
		order = append(order, c.ID)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]change, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// runRollback restores each logged title. A row is only written if its title
// is still the one the tool set, so later manual edits are never clobbered.
func runRollback(ctx context.Context, db *dynamodb.Client, cfg *Config) bool {
	changes, err := readChanges(cfg.ChangeLog, cfg.TableName)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("change log %s not found; rebuilding original titles from %s\n", cfg.ChangeLog, cfg.InputFile)
		changes, err = changesFromInput(ctx, db, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rollback:", err)
		return false
	}
	fmt.Printf("rollback table=%s log=%s changes=%d dry_run=%v\n", cfg.TableName, cfg.ChangeLog, len(changes), cfg.DryRun)

	var restored, skipped, failed atomic.Int64
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Printf(format+"\n", a...)
	}

	work := make(chan change)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				if cfg.DryRun {
					logf("DRY  %s: %q -> %q", c.Identifier, c.NewTitle, c.OldTitle)
					restored.Add(1)
					continue
				}
				_, err := db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:                aws.String(cfg.TableName),
					Key:                      map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: c.ID}},
					UpdateExpression:         aws.String("SET #t = :old"),
					ConditionExpression:      aws.String("#t = :new"),
					ExpressionAttributeNames: map[string]string{"#t": "title"},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":old": &types.AttributeValueMemberS{Value: c.OldTitle},
						":new": &types.AttributeValueMemberS{Value: c.NewTitle},
					},
				})
				var ccf *types.ConditionalCheckFailedException
				switch {
				case errors.As(err, &ccf):
					logf("SKIP %s: title is no longer %q", c.Identifier, c.NewTitle)
					skipped.Add(1)
				case err != nil:
					logf("FAIL %s: %v", c.Identifier, err)
					failed.Add(1)
				default:
					logf("OK   %s: %q -> %q", c.Identifier, c.NewTitle, c.OldTitle)
					restored.Add(1)
				}
			}
		}()
	}
	for _, c := range changes {
		work <- c
	}
	close(work)
	wg.Wait()

	fmt.Printf("done: restored=%d skipped=%d failed=%d\n", restored.Load(), skipped.Load(), failed.Load())
	return failed.Load() == 0
}

// changesFromInput rebuilds the change list from the input report using the
// same filters and suffix as a normal run, for rollback without a change log.
func changesFromInput(ctx context.Context, db *dynamodb.Client, cfg *Config) ([]change, error) {
	if err := cfg.validateApply(); err != nil {
		return nil, fmt.Errorf("change log not found and cannot fall back to input file: %w", err)
	}
	jobs, err := plan(cfg)
	if err != nil {
		return nil, err
	}
	ids, err := idsByIdentifier(ctx, db, cfg)
	if err != nil {
		return nil, err
	}
	var out []change
	for _, j := range jobs {
		found := ids[j.identifier]
		if len(found) != 1 {
			fmt.Printf("SKIP %s: %d table rows match identifier\n", j.identifier, len(found))
			continue
		}
		out = append(out, change{ID: found[0], Identifier: j.identifier, Table: cfg.TableName, OldTitle: j.oldTitle, NewTitle: j.newTitle})
	}
	return out, nil
}
