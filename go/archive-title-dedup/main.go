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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Region     string `yaml:"region"`
	TableName  string `yaml:"table_name"`
	OutputDir  string `yaml:"output_dir"`
	OutputFile string `yaml:"output_file"`
	// Concurrency is the number of parallel scan segments (one goroutine each).
	Concurrency int `yaml:"concurrency"`
}

type Report struct {
	Duplicates []Group `json:"duplicates"`
}

type Group struct {
	Title   string   `json:"title"`
	Records []Record `json:"records"`
}

type Record struct {
	Identifier       string  `json:"identifier"`
	ParentCollection *string `json:"parent_collection"`
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
func scanSegment(ctx context.Context, db *dynamodb.Client, table string, seg, total int) (map[string][]Record, int, error) {
	byTitle := map[string][]Record{}
	scanned := 0
	in := &dynamodb.ScanInput{
		TableName:            aws.String(table),
		ProjectionExpression: aws.String("#t, #i, #p"),
		ExpressionAttributeNames: map[string]string{
			"#t": "title", "#i": "identifier", "#p": "parent_collection",
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
			byTitle[title] = append(byTitle[title], Record{Identifier: ident, ParentCollection: firstParent(it)})
		}
	}
	return byTitle, scanned, nil
}

// findDuplicates scans the table with one goroutine per segment and groups
// records by exact title, keeping only titles that occur more than once.
func findDuplicates(ctx context.Context, db *dynamodb.Client, table string, workers int) (*Report, int, error) {
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
			part, n, err := scanSegment(ctx, db, table, seg, workers)
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

// usage lists the available commands and exits 2, matching the flag
// package's own convention for a bad invocation.
func usage() {
	fmt.Fprintln(os.Stderr, "usage: archive-title-dedup <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  report   scan the table and write the duplicate-titles report")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch cmd := os.Args[1]; cmd {
	case "report":
		runReport(os.Args[2:])
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

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws:", err)
		os.Exit(1)
	}
	db := dynamodb.NewFromConfig(awsCfg)

	rep, scanned, err := findDuplicates(ctx, db, cfg.TableName, cfg.Concurrency)
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
