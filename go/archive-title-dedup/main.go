package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	// CollectionTableName is queried for the collection's own identifier when
	// a record has no parent_collection_identifier. Required when
	// collection_identifier is set.
	CollectionTableName string `yaml:"collection_table_name"`
	Suffix              string `yaml:"suffix"`
	Concurrency         int    `yaml:"concurrency"`
	DryRun              bool   `yaml:"dry_run"`
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
	switch {
	case c.TableName == "":
		return nil, errors.New("table_name is required")
	case c.InputFile == "":
		return nil, errors.New("input_file is required")
	case c.ItemCategory == "":
		return nil, errors.New("item_category is required")
	case c.Suffix == "":
		return nil, errors.New("suffix is required")
	}
	if c.CollectionIdentifier != "" && c.CollectionTableName == "" {
		return nil, errors.New("collection_table_name is required when collection_identifier is set")
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.Concurrency < 1 {
		c.Concurrency = 10
	}
	c.InputFile = expandHome(c.InputFile)
	return &c, nil
}

// plan builds the list of title changes: every record in a duplicate group
// whose item_category matches gets "<title><suffix>-<n>", n starting at 1
// within the group.
//
// lookup returns the identifier of the Collection row with the given id; it is
// used when a record has no parent_collection_identifier.
func plan(cfg *Config, lookup func(collectionID string) (string, error)) ([]job, error) {
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
		n := 0
		for _, r := range g.Records {
			if r.ItemCategory != cfg.ItemCategory {
				continue
			}
			if cfg.CollectionIdentifier != "" {
				var want string
				if r.ParentCollectionIdentifier != nil {
					want = *r.ParentCollectionIdentifier
				} else if r.CollectionID != "" {
					id, err := lookup(r.CollectionID)
					if err != nil {
						return nil, fmt.Errorf("collection %s (record %s): %w", r.CollectionID, r.Identifier, err)
					}
					want = id
				}
				if want != cfg.CollectionIdentifier {
					continue
				}
			}
			n++
			jobs = append(jobs, job{
				identifier: r.Identifier,
				oldTitle:   g.Title,
				newTitle:   fmt.Sprintf("%s%s-%d", g.Title, cfg.Suffix, n),
			})
		}
	}
	return jobs, nil
}

// idsByIdentifier scans the table once and maps identifier -> id(s), since
// the table is keyed on id while the report only carries identifiers.
func idsByIdentifier(ctx context.Context, db *dynamodb.Client, table string) (map[string][]string, error) {
	out := map[string][]string{}
	p := dynamodb.NewScanPaginator(db, &dynamodb.ScanInput{
		TableName:                aws.String(table),
		ProjectionExpression:     aws.String("#i, #k"),
		ExpressionAttributeNames: map[string]string{"#i": "identifier", "#k": "id"},
	})
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
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if *dry {
		cfg.DryRun = true
	}

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws:", err)
		os.Exit(1)
	}
	db := dynamodb.NewFromConfig(awsCfg)

	cache := map[string]string{}
	lookup := func(collectionID string) (string, error) {
		if v, ok := cache[collectionID]; ok {
			return v, nil
		}
		out, err := db.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:                aws.String(cfg.CollectionTableName),
			Key:                      map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: collectionID}},
			ProjectionExpression:     aws.String("#i"),
			ExpressionAttributeNames: map[string]string{"#i": "identifier"},
		})
		if err != nil {
			return "", err
		}
		v := ""
		if s, ok := out.Item["identifier"].(*types.AttributeValueMemberS); ok {
			v = s.Value
		}
		cache[collectionID] = v
		return v, nil
	}

	jobs, err := plan(cfg, lookup)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		os.Exit(1)
	}
	fmt.Printf("table=%s category=%q collection_identifier=%q planned=%d dry_run=%v\n", cfg.TableName, cfg.ItemCategory, cfg.CollectionIdentifier, len(jobs), cfg.DryRun)

	ids, err := idsByIdentifier(ctx, db, cfg.TableName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
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
	if failed.Load() > 0 {
		os.Exit(1)
	}
}
