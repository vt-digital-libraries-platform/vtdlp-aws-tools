// Command iiif-infoFile-modifier finds every "info.json" object belonging to
// archives in a DynamoDB-tracked collection. It looks up the Collection
// record (by identifier) in a configured DynamoDB table to get the
// collection's id, queries the Archive table for every record whose
// collection matches that id, and for each archive identifier lists
// the "info.json" object(s) under the collection's default per-item tile
// layout (<collection_prefix>/<collection_identifier>/<tiles_dir_name>/
// <archive_identifier>-<index>/info.json). For each one found, it writes a
// "backup_info.json" copy at the same key location, then rewrites the
// info.json object's JSON content to match a target format and writes it
// back to its original key.
//
// Run with -rollback to reverse that: for each discovered info.json, its
// backup_info.json is verified to be a valid pre-transform object and then
// copied over the info.json key (restoring the true original in place),
// after which the backup_info.json is deleted.
//
// Run with -all to ignore collection_identifier and instead process every
// Collection record in collection_table whose "visible" attribute is true,
// one after another; a failure in one collection doesn't stop the rest from
// being attempted, and the process exits non-zero if any collection had a
// failure.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gopkg.in/yaml.v3"
)

// defaultConcurrency is how many S3/DynamoDB requests run in parallel when
// concurrency isn't set in the config.
const defaultConcurrency = 10

type Config struct {
	Region               string `yaml:"region"`
	Bucket               string `yaml:"bucket"`
	CollectionPrefix     string `yaml:"collection_prefix"`
	InfoFileName         string `yaml:"info_file_name"`
	BackupFileName       string `yaml:"backup_file_name"`
	TilesDirName         string `yaml:"tiles_dir_name"`
	CollectionTable      string `yaml:"collection_table"`
	ArchiveTable         string `yaml:"archive_table"`
	CollectionIdentifier string `yaml:"collection_identifier"`
	Concurrency          int    `yaml:"concurrency"`
	DryRun               bool   `yaml:"dry_run"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if cfg.InfoFileName == "" {
		cfg.InfoFileName = "info.json"
	}
	if cfg.BackupFileName == "" {
		cfg.BackupFileName = "backup_info.json"
	}
	if cfg.TilesDirName == "" {
		cfg.TilesDirName = "tiles"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}

	var missing []string
	if cfg.Region == "" {
		missing = append(missing, "region")
	}
	if cfg.Bucket == "" {
		missing = append(missing, "bucket")
	}
	if cfg.CollectionPrefix == "" {
		missing = append(missing, "collection_prefix")
	}
	if cfg.CollectionTable == "" {
		missing = append(missing, "collection_table")
	}
	if cfg.ArchiveTable == "" {
		missing = append(missing, "archive_table")
	}
	// collection_identifier is required unless -all is passed (checked in
	// main, which knows about CLI flags); loadConfig doesn't enforce it.
	if len(missing) > 0 {
		return nil, fmt.Errorf("config file is missing required field(s): %s", strings.Join(missing, ", "))
	}

	if !strings.HasSuffix(cfg.CollectionPrefix, "/") {
		cfg.CollectionPrefix += "/"
	}

	return &cfg, nil
}

// requiredContext, requiredProtocol, and requiredProfile are fixed values
// every corrected "tiles/" info.json declares, regardless of what the input
// file had; the tiler behind these files always speaks IIIF Image API 2
// level0, serving JPEG tiles at default quality.
const requiredContext = "http://iiif.io/api/image/2/context.json"
const requiredProtocol = "http://iiif.io/api/image"

var requiredProfile = []interface{}{
	"http://iiif.io/api/image/2/level0.json",
	profileExtra{
		Formats:   []string{"jpg"},
		Qualities: []string{"default"},
		Supports:  []string{"cors", "baseUriRedirect"},
	},
}

type profileExtra struct {
	Formats   []string `json:"formats"`
	Qualities []string `json:"qualities"`
	Supports  []string `json:"supports"`
}

// inputInfo is the shape of a "tiles/" info.json as originally written; only
// @id, width, height, and tiles are read from it, everything else is
// replaced with fixed corrected values.
type inputInfo struct {
	ID     string      `json:"@id"`
	Width  int         `json:"width"`
	Height int         `json:"height"`
	Tiles  []inputTile `json:"tiles"`
}

type inputTile struct {
	Width        int   `json:"width"`
	ScaleFactors []int `json:"scaleFactors"`
}

// outputInfo is the corrected shape: no "sizes" array, fixed
// @context/protocol/profile, and tiles[] entries reordered to
// scaleFactors, then width.
type outputInfo struct {
	Context  string        `json:"@context"`
	ID       string        `json:"@id"`
	Profile  []interface{} `json:"profile"`
	Protocol string        `json:"protocol"`
	Tiles    []outputTile  `json:"tiles"`
	Width    int           `json:"width"`
	Height   int           `json:"height"`
}

type outputTile struct {
	ScaleFactors []int `json:"scaleFactors"`
	Width        int   `json:"width"`
}

// transformInfoJSON converts a "tiles/" info.json object's raw bytes from
// its original format to the corrected target format: @context, protocol,
// and profile are set to fixed values, the "sizes" array is dropped, and
// each tiles[] entry is reordered to scaleFactors, then width. @id, width,
// height, and tile values are preserved from the input.
func transformInfoJSON(data []byte) ([]byte, error) {
	var in inputInfo
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("parsing info.json: %w", err)
	}

	tiles := make([]outputTile, 0, len(in.Tiles))
	for _, t := range in.Tiles {
		tiles = append(tiles, outputTile{
			ScaleFactors: t.ScaleFactors,
			Width:        t.Width,
		})
	}

	out := outputInfo{
		Context:  requiredContext,
		ID:       in.ID,
		Profile:  requiredProfile,
		Protocol: requiredProtocol,
		Tiles:    tiles,
		Width:    in.Width,
		Height:   in.Height,
	}

	newData, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("re-marshaling info.json: %w", err)
	}
	return append(newData, '\n'), nil
}

// isAlreadyTransformed reports whether data's info.json content already
// matches the corrected/output format, so callers can avoid re-backing-up
// and re-transforming it: specifically, whether its profile's second
// (feature) element declares a "formats" key, which requiredProfile always
// adds and the original pre-transform format never has. Running the tool
// twice against the same collection without this check would back up an
// already-corrected file over top of the real original, destroying data
// (e.g. the "sizes" array) that can't be recovered afterward.
func isAlreadyTransformed(data []byte) (bool, error) {
	var probe struct {
		Profile []json.RawMessage `json:"profile"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, fmt.Errorf("parsing info.json: %w", err)
	}
	if len(probe.Profile) < 2 {
		return false, nil
	}

	var extra map[string]json.RawMessage
	if err := json.Unmarshal(probe.Profile[1], &extra); err != nil {
		return false, nil
	}

	_, hasFormats := extra["formats"]
	return hasFormats, nil
}

// isPreTransformShape reports whether data looks like a valid pre-transform
// info.json: it must have a non-empty "sizes" array, and its profile's
// second (feature) element must not declare "formats" (the tool's own
// output always drops "sizes" and always adds "formats" — see
// isAlreadyTransformed). -rollback uses this to confirm a backup_info.json
// is safe to restore before overwriting info.json with it.
func isPreTransformShape(data []byte) (bool, error) {
	var probe struct {
		Sizes   []json.RawMessage `json:"sizes"`
		Profile []json.RawMessage `json:"profile"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, fmt.Errorf("parsing info.json: %w", err)
	}
	if len(probe.Sizes) == 0 || len(probe.Profile) < 2 {
		return false, nil
	}

	var extra map[string]json.RawMessage
	if err := json.Unmarshal(probe.Profile[1], &extra); err != nil {
		return false, nil
	}

	_, hasFormats := extra["formats"]
	return !hasFormats, nil
}

// runConcurrent calls fn(i) once for every i in [0, n), running up to
// concurrency calls at a time, and blocks until all have returned. fn is
// responsible for recording its own result (e.g. by writing to index i of a
// pre-sized slice) since callers are expected to run purely independent,
// index-addressable work items (S3/DynamoDB requests) through this.
func runConcurrent(n, concurrency int, fn func(i int)) {
	if n == 0 {
		return
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > n {
		concurrency = n
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// findCollectionID scans collectionTable for the item whose "identifier"
// attribute equals collectionIdentifier and returns that item's "id"
// attribute value. Returns an error if zero or more than one match is
// found, or if the matching record has no string "id" attribute.
func findCollectionID(ctx context.Context, client *dynamodb.Client, collectionTable, collectionIdentifier string) (string, error) {
	var found []map[string]types.AttributeValue

	paginator := dynamodb.NewScanPaginator(client, &dynamodb.ScanInput{
		TableName:        aws.String(collectionTable),
		FilterExpression: aws.String("identifier = :identifier"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":identifier": &types.AttributeValueMemberS{Value: collectionIdentifier},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("scanning collection table %q: %w", collectionTable, err)
		}
		found = append(found, page.Items...)
	}

	if len(found) == 0 {
		return "", fmt.Errorf("no collection found in table %q with identifier %q", collectionTable, collectionIdentifier)
	}
	if len(found) > 1 {
		return "", fmt.Errorf("multiple (%d) collections found in table %q with identifier %q", len(found), collectionTable, collectionIdentifier)
	}

	idAttr, ok := found[0]["id"].(*types.AttributeValueMemberS)
	if !ok || idAttr.Value == "" {
		return "", fmt.Errorf("collection record (identifier=%q) in table %q is missing a string \"id\" field", collectionIdentifier, collectionTable)
	}

	return idAttr.Value, nil
}

// collectionTarget identifies a single collection to process: its
// DynamoDB id (used to look up archives) and its identifier (used as the
// S3 collection-root path segment).
type collectionTarget struct {
	id         string
	identifier string
}

// findVisibleCollections scans collectionTable for every item whose
// "visible" attribute is boolean true, and returns each match's "id" and
// "identifier" attribute values as a collectionTarget. Used by -all to
// process every visible collection instead of a single configured one.
// Items missing a valid string "id" or "identifier" are skipped with a
// logged warning rather than aborting the whole run.
func findVisibleCollections(ctx context.Context, client *dynamodb.Client, collectionTable string) ([]collectionTarget, error) {
	var targets []collectionTarget

	paginator := dynamodb.NewScanPaginator(client, &dynamodb.ScanInput{
		TableName:        aws.String(collectionTable),
		FilterExpression: aws.String("visible = :visible"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":visible": &types.AttributeValueMemberBOOL{Value: true},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scanning collection table %q: %w", collectionTable, err)
		}
		for _, item := range page.Items {
			idAttr, ok := item["id"].(*types.AttributeValueMemberS)
			if !ok || idAttr.Value == "" {
				log.Printf("WARNING: visible collection record in table %q is missing a string \"id\" field, skipping", collectionTable)
				continue
			}
			identifierAttr, ok := item["identifier"].(*types.AttributeValueMemberS)
			if !ok || identifierAttr.Value == "" {
				log.Printf("WARNING: visible collection record (id=%q) in table %q is missing a string \"identifier\" field, skipping", idAttr.Value, collectionTable)
				continue
			}
			targets = append(targets, collectionTarget{id: idAttr.Value, identifier: identifierAttr.Value})
		}
	}

	return targets, nil
}

// findArchiveIdentifiers scans archiveTable for every item whose
// "collection" attribute equals collectionID and returns the deduplicated
// set of matching items' "identifier" attribute values. Items missing a
// valid string "identifier" are skipped with a logged warning rather than
// aborting the whole run.
func findArchiveIdentifiers(ctx context.Context, client *dynamodb.Client, archiveTable, collectionID string) ([]string, error) {
	seen := make(map[string]struct{})
	var identifiers []string

	paginator := dynamodb.NewScanPaginator(client, &dynamodb.ScanInput{
		TableName:        aws.String(archiveTable),
		FilterExpression: aws.String("#collection = :collection"),
		ExpressionAttributeNames: map[string]string{
			"#collection": "collection",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":collection": &types.AttributeValueMemberS{Value: collectionID},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scanning archive table %q: %w", archiveTable, err)
		}
		for _, item := range page.Items {
			idAttr, ok := item["identifier"].(*types.AttributeValueMemberS)
			if !ok || idAttr.Value == "" {
				log.Printf("WARNING: archive record in table %q (collection=%q) missing a string \"identifier\" field, skipping", archiveTable, collectionID)
				continue
			}
			if _, dup := seen[idAttr.Value]; dup {
				continue
			}
			seen[idAttr.Value] = struct{}{}
			identifiers = append(identifiers, idAttr.Value)
		}
	}

	return identifiers, nil
}

// findInfoObjects lists, for each archive identifier, every object under
// tilesPrefix in bucket whose key matches the default per-item layout
// tilesPrefix + "<archive_identifier>-<index>/" + infoFileName, i.e. exactly
// one directory segment (the archive's tile directory) between tilesPrefix
// and the filename. Each archive identifier may have more than one matching
// "-<index>" subdirectory, or none at all (e.g. tiles not yet generated).
// The IIIF spec places other, differently-formatted info.json files outside
// of tilesPrefix (e.g. presentation manifests); those are intentionally left
// alone.
func findInfoObjects(ctx context.Context, client *s3.Client, bucket, tilesPrefix, infoFileName string, archiveIdentifiers []string, concurrency int) ([]string, error) {
	type listResult struct {
		keys []string
		err  error
	}
	results := make([]listResult, len(archiveIdentifiers))

	runConcurrent(len(archiveIdentifiers), concurrency, func(i int) {
		identifierPrefix := tilesPrefix + archiveIdentifiers[i] + "-"

		var keys []string
		paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket),
			Prefix: aws.String(identifierPrefix),
		})

		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				results[i] = listResult{err: fmt.Errorf("listing objects under %q: %w", identifierPrefix, err)}
				return
			}
			for _, obj := range page.Contents {
				key := aws.ToString(obj.Key)
				rest := strings.TrimPrefix(key, tilesPrefix)
				segs := strings.Split(rest, "/")
				if len(segs) != 2 || segs[0] == "" || segs[1] != infoFileName {
					continue
				}
				keys = append(keys, key)
			}
		}
		results[i] = listResult{keys: keys}
	})

	var keys []string
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		keys = append(keys, r.keys...)
	}
	return keys, nil
}

// backupKeyFor returns the backup object key that sits alongside infoKey at
// the same S3 "directory" (key prefix).
func backupKeyFor(infoKey, infoFileName, backupFileName string) string {
	dir := strings.TrimSuffix(infoKey, infoFileName)
	return dir + backupFileName
}

// copyObject creates dstKey as a server-side copy of srcKey within the same
// bucket.
func copyObject(ctx context.Context, client *s3.Client, bucket, srcKey, dstKey string) error {
	copySource := url.PathEscape(bucket + "/" + srcKey)
	_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(copySource),
	})
	if err != nil {
		return fmt.Errorf("copying %q to %q: %w", srcKey, dstKey, err)
	}
	return nil
}

// downloadObject fetches the full contents of key in bucket.
func downloadObject(ctx context.Context, client *s3.Client, bucket, key string) ([]byte, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("getting %q: %w", key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", key, err)
	}
	return data, nil
}

// uploadObject writes data to key in bucket, overwriting any existing
// object at that key.
func uploadObject(ctx context.Context, client *s3.Client, bucket, key string, data []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("putting %q: %w", key, err)
	}
	return nil
}

// deleteObject removes key from bucket.
func deleteObject(ctx context.Context, client *s3.Client, bucket, key string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting %q: %w", key, err)
	}
	return nil
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	dryRunFlag := flag.Bool("dry-run", false, "force dry-run mode (scan and report only, no writes); overrides dry_run: false in the config file")
	rollback := flag.Bool("rollback", false, "reverse mode: convert each discovered info.json from the corrected/output format back to the original input format, then delete its backup_info.json")
	all := flag.Bool("all", false, "ignore collection_identifier and process every Collection record in collection_table where visible=true")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if !*all && cfg.CollectionIdentifier == "" {
		log.Fatalf("config error: collection_identifier is required unless -all is passed")
	}

	dryRun := cfg.DryRun || *dryRunFlag

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		log.Fatalf("loading AWS config: %v", err)
	}
	client := s3.NewFromConfig(awsCfg)
	ddbClient := dynamodb.NewFromConfig(awsCfg)

	mode := "LIVE (objects will be written)"
	if dryRun {
		mode = "DRY RUN (no objects will be written)"
	}
	if *rollback {
		mode = "ROLLBACK " + mode
	}
	log.Printf("bucket=%s info_file_name=%s backup_file_name=%s concurrency=%d mode=%s",
		cfg.Bucket, cfg.InfoFileName, cfg.BackupFileName, cfg.Concurrency, mode)

	var targets []collectionTarget
	if *all {
		targets, err = findVisibleCollections(ctx, ddbClient, cfg.CollectionTable)
		if err != nil {
			log.Fatalf("%v", err)
		}
		log.Printf("found %d visible collection(s) in table %s", len(targets), cfg.CollectionTable)
	} else {
		collectionID, err := findCollectionID(ctx, ddbClient, cfg.CollectionTable, cfg.CollectionIdentifier)
		if err != nil {
			log.Fatalf("%v", err)
		}
		log.Printf("found collection id=%s for collection_identifier=%s in table %s", collectionID, cfg.CollectionIdentifier, cfg.CollectionTable)
		targets = []collectionTarget{{id: collectionID, identifier: cfg.CollectionIdentifier}}
	}

	var totalFailed int
	for _, target := range targets {
		collectionRootPrefix := cfg.CollectionPrefix + target.identifier + "/"
		tilesPrefix := collectionRootPrefix + cfg.TilesDirName + "/"

		log.Printf("--- collection identifier=%s id=%s tiles_prefix=%s ---", target.identifier, target.id, tilesPrefix)

		archiveIdentifiers, err := findArchiveIdentifiers(ctx, ddbClient, cfg.ArchiveTable, target.id)
		if err != nil {
			log.Printf("ERROR %v", err)
			totalFailed++
			continue
		}
		log.Printf("found %d archive identifier(s) in table %s for collection id=%s", len(archiveIdentifiers), cfg.ArchiveTable, target.id)

		infoKeys, err := findInfoObjects(ctx, client, cfg.Bucket, tilesPrefix, cfg.InfoFileName, archiveIdentifiers, cfg.Concurrency)
		if err != nil {
			log.Printf("ERROR %v", err)
			totalFailed++
			continue
		}
		log.Printf("found %d %s object(s) under %s<archive_identifier>-<index>/", len(infoKeys), cfg.InfoFileName, tilesPrefix)

		if *rollback {
			totalFailed += runRollback(ctx, client, cfg, infoKeys, dryRun)
		} else {
			totalFailed += runTransform(ctx, client, cfg, infoKeys, dryRun)
		}
	}

	if len(targets) > 1 {
		log.Printf("all done: collections=%d total_failed=%d", len(targets), totalFailed)
	}
	if totalFailed > 0 {
		os.Exit(1)
	}
}

// pendingTransform is an info.json object confirmed (by isAlreadyTransformed)
// to still be in the original pre-transform format, and therefore safe to
// back up and rewrite.
type pendingTransform struct {
	key       string
	backupKey string
	data      []byte
}

// runTransform backs up, then transforms and writes back, each object in
// infoKeys that isn't already in the corrected/output format (the normal,
// forward mode of operation). Objects already in the corrected format are
// skipped entirely — neither backed up nor rewritten — so that running the
// tool again over an already-processed collection can't overwrite a
// backup_info.json with already-corrected content. Returns the number of
// keys that failed, so the caller can aggregate failures across multiple
// collections (-all) instead of exiting immediately.
func runTransform(ctx context.Context, client *s3.Client, cfg *Config, infoKeys []string, dryRun bool) int {
	// Phase 1: download and classify each key concurrently.
	type classifyResult struct {
		err     error
		skip    bool
		pending pendingTransform
	}
	classified := make([]classifyResult, len(infoKeys))
	runConcurrent(len(infoKeys), cfg.Concurrency, func(i int) {
		infoKey := infoKeys[i]
		data, err := downloadObject(ctx, client, cfg.Bucket, infoKey)
		if err != nil {
			classified[i] = classifyResult{err: fmt.Errorf("downloading %q: %w", infoKey, err)}
			return
		}

		alreadyTransformed, err := isAlreadyTransformed(data)
		if err != nil {
			classified[i] = classifyResult{err: fmt.Errorf("inspecting %q: %w", infoKey, err)}
			return
		}
		if alreadyTransformed {
			classified[i] = classifyResult{skip: true}
			return
		}

		classified[i] = classifyResult{pending: pendingTransform{
			key:       infoKey,
			backupKey: backupKeyFor(infoKey, cfg.InfoFileName, cfg.BackupFileName),
			data:      data,
		}}
	})

	var toProcess []pendingTransform
	var skipped, failed int
	for i, r := range classified {
		switch {
		case r.err != nil:
			failed++
			log.Printf("ERROR %v", r.err)
		case r.skip:
			skipped++
			log.Printf("skipping %q: already in corrected format, leaving its backup untouched", infoKeys[i])
		default:
			toProcess = append(toProcess, r.pending)
		}
	}

	// Phase 2: back up every pending item concurrently; abort before any
	// modification if even one backup fails.
	backupErrs := make([]error, len(toProcess))
	if dryRun {
		for _, p := range toProcess {
			log.Printf("[DRY RUN] would back up %q -> %q", p.key, p.backupKey)
		}
	} else {
		runConcurrent(len(toProcess), cfg.Concurrency, func(i int) {
			p := toProcess[i]
			if err := copyObject(ctx, client, cfg.Bucket, p.key, p.backupKey); err != nil {
				backupErrs[i] = fmt.Errorf("backing up %q: %w", p.key, err)
			}
		})
	}

	var backedUp, backupFailed int
	for i, err := range backupErrs {
		if err != nil {
			backupFailed++
			log.Printf("ERROR %v", err)
			continue
		}
		if !dryRun {
			backedUp++
			log.Printf("backed up %q -> %q", toProcess[i].key, toProcess[i].backupKey)
		}
	}
	failed += backupFailed

	if backupFailed > 0 {
		log.Printf("aborting modification step for this collection: %d backup(s) failed", backupFailed)
		log.Printf("done: found=%d skipped=%d backed_up=%d modified=0 failed=%d", len(infoKeys), skipped, backedUp, failed)
		return failed
	}

	// Phase 3: transform and upload every pending item concurrently.
	uploadErrs := make([]error, len(toProcess))
	if !dryRun {
		runConcurrent(len(toProcess), cfg.Concurrency, func(i int) {
			p := toProcess[i]
			newData, err := transformInfoJSON(p.data)
			if err != nil {
				uploadErrs[i] = fmt.Errorf("transforming %q: %w", p.key, err)
				return
			}
			if err := uploadObject(ctx, client, cfg.Bucket, p.key, newData); err != nil {
				uploadErrs[i] = fmt.Errorf("uploading %q: %w", p.key, err)
			}
		})
	}

	var modified int
	for i, p := range toProcess {
		if dryRun {
			newData, err := transformInfoJSON(p.data)
			if err != nil {
				failed++
				log.Printf("ERROR transforming %q: %v", p.key, err)
				continue
			}
			log.Printf("[DRY RUN] would write modified %q (%d bytes -> %d bytes)", p.key, len(p.data), len(newData))
			continue
		}
		if err := uploadErrs[i]; err != nil {
			failed++
			log.Printf("ERROR %v", err)
			continue
		}
		modified++
		log.Printf("modified %q", p.key)
	}

	if dryRun {
		log.Printf("done: found=%d skipped=%d (dry run, nothing written)", len(infoKeys), skipped)
	} else {
		log.Printf("done: found=%d skipped=%d backed_up=%d modified=%d failed=%d", len(infoKeys), skipped, backedUp, modified, failed)
	}

	return failed
}

// runRollback restores each object in infoKeys from its backup_info.json:
// after confirming the backup is a valid pre-transform object
// (isPreTransformShape), it copies backupKey over infoKey (a server-side
// S3 "move" of the true original, rather than reconstructing one from
// hardcoded field values) and then deletes backupKey. Returns the number
// of keys that failed, so the caller can aggregate failures across
// multiple collections (-all) instead of exiting immediately.
func runRollback(ctx context.Context, client *s3.Client, cfg *Config, infoKeys []string, dryRun bool) int {
	type rollbackResult struct {
		err        error
		dryRunNote string
		restored   bool
	}
	results := make([]rollbackResult, len(infoKeys))

	runConcurrent(len(infoKeys), cfg.Concurrency, func(i int) {
		infoKey := infoKeys[i]
		backupKey := backupKeyFor(infoKey, cfg.InfoFileName, cfg.BackupFileName)

		backupData, err := downloadObject(ctx, client, cfg.Bucket, backupKey)
		if err != nil {
			results[i] = rollbackResult{err: fmt.Errorf("downloading backup %q: %w", backupKey, err)}
			return
		}

		valid, err := isPreTransformShape(backupData)
		if err != nil {
			results[i] = rollbackResult{err: fmt.Errorf("checking backup %q: %w", backupKey, err)}
			return
		}
		if !valid {
			results[i] = rollbackResult{err: fmt.Errorf("backup %q does not look like a pre-transform info.json, refusing to restore it over %q", backupKey, infoKey)}
			return
		}

		if dryRun {
			results[i] = rollbackResult{dryRunNote: fmt.Sprintf("[DRY RUN] would restore %q from backup %q, then remove the backup", infoKey, backupKey)}
			return
		}

		if err := copyObject(ctx, client, cfg.Bucket, backupKey, infoKey); err != nil {
			results[i] = rollbackResult{err: fmt.Errorf("restoring %q from backup %q: %w", infoKey, backupKey, err)}
			return
		}

		if err := deleteObject(ctx, client, cfg.Bucket, backupKey); err != nil {
			results[i] = rollbackResult{err: fmt.Errorf("removing backup %q: %w", backupKey, err)}
			return
		}

		results[i] = rollbackResult{restored: true}
	})

	var rolledBack, failed int
	for i, r := range results {
		infoKey := infoKeys[i]
		backupKey := backupKeyFor(infoKey, cfg.InfoFileName, cfg.BackupFileName)
		switch {
		case r.err != nil:
			failed++
			log.Printf("ERROR %v", r.err)
		case r.dryRunNote != "":
			log.Print(r.dryRunNote)
		case r.restored:
			rolledBack++
			log.Printf("restored %q from backup, removed backup %q", infoKey, backupKey)
		}
	}

	if dryRun {
		log.Printf("done: found=%d (dry run, nothing written)", len(infoKeys))
	} else {
		log.Printf("done: found=%d rolled_back=%d failed=%d", len(infoKeys), rolledBack, failed)
	}

	return failed
}
