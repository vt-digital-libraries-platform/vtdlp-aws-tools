package main

import (
	"context"
	"fmt"
	"log"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// oldDomain is the stale CDN host still embedded in most IAWA IIIF JSON
// docs. newDomain is the correct CloudFront distribution for this bucket.
const (
	oldDomain = "img.cloud.lib.vt.edu"
	newDomain = "d4qp6z580yvb5.cloudfront.net"
)

// urlRe matches an embedded asset URL under /iawa/<collection>/... using
// either the old or the new domain, so rewriting is idempotent: re-running
// against an already-fixed document is a safe no-op.
var urlRe = regexp.MustCompile(`https://(?:` + regexp.QuoteMeta(oldDomain) + `|` + regexp.QuoteMeta(newDomain) + `)/iawa/([^/"]+)/([^"]*)`)

// rewriteTask is one JSON document to inspect/rewrite in place.
type rewriteTask struct {
	Key        string // full S3 key
	Collection string
	ItemName   string // "" for docs with no single-item context (none currently, kept for clarity)
}

// discoverJSONFiles lists every manifest.json, info.json, canvas/*.json,
// annotation/*.json, and sequence/*.json object under the collection root
// and classifies each into a rewriteTask.
func discoverJSONFiles(ctx context.Context, client s3Lister, bucket, collection string) ([]rewriteTask, error) {
	prefix := "iawa/" + collection + "/"
	objs, err := client.ListAllKeys(ctx, bucket, prefix)
	if err != nil {
		return nil, err
	}

	var tasks []rewriteTask
	for _, o := range objs {
		base := path.Base(o.Key)
		dir := path.Dir(o.Key)
		rel := strings.TrimPrefix(dir, prefix)

		switch {
		case base == "info.json":
			// iawa/<collection>/tiles/<TileID>/info.json
			if !strings.HasPrefix(rel, "tiles/") {
				continue
			}
			tasks = append(tasks, rewriteTask{Key: o.Key, Collection: collection})

		case base == "manifest.json":
			// iawa/<collection>/<ItemName>/manifest.json
			if rel == "" || strings.Contains(rel, "/") {
				continue // not directly under an item dir at the collection root
			}
			tasks = append(tasks, rewriteTask{Key: o.Key, Collection: collection, ItemName: rel})

		case strings.HasSuffix(base, ".json") && (strings.HasSuffix(rel, "/canvas") || strings.HasSuffix(rel, "/annotation") || strings.HasSuffix(rel, "/sequence")):
			// iawa/<collection>/<ItemName>/{canvas,annotation,sequence}/<page>.json
			parts := strings.Split(rel, "/")
			if len(parts) != 2 {
				continue
			}
			tasks = append(tasks, rewriteTask{Key: o.Key, Collection: collection, ItemName: parts[0]})
		}
	}
	return tasks, nil
}

// rewriteURLs rewrites every embedded asset URL in content to the correct
// CloudFront domain and collection-root-relative path, using t's own key
// context (Collection, ItemName) as ground truth for where the asset
// actually lives now. Returns the rewritten content, whether anything
// changed, and any warnings (references that couldn't be confidently
// rewritten and were left untouched).
func rewriteURLs(content []byte, t rewriteTask) (out []byte, changed bool, warnings []string) {
	changed = false
	result := urlRe.ReplaceAllStringFunc(string(content), func(match string) string {
		groups := urlRe.FindStringSubmatch(match)
		urlColl, urlRest := groups[1], groups[2]

		if urlColl != t.Collection {
			warnings = append(warnings, fmt.Sprintf("%s: URL references collection %q, leaving untouched: %s", t.Key, urlColl, match))
			return match
		}

		var newRest string
		if idx := strings.Index(urlRest, "tiles/"); idx >= 0 {
			newRest = urlRest[idx:]
		} else if t.ItemName != "" {
			needle := t.ItemName + "/"
			idx := strings.LastIndex(urlRest, needle)
			if idx < 0 {
				warnings = append(warnings, fmt.Sprintf("%s: item-relative URL doesn't contain item name %q, leaving untouched: %s", t.Key, t.ItemName, match))
				return match
			}
			newRest = urlRest[idx:]
		} else {
			warnings = append(warnings, fmt.Sprintf("%s: URL has no tiles/ segment and no item context, leaving untouched: %s", t.Key, match))
			return match
		}

		rewritten := "https://" + newDomain + "/iawa/" + t.Collection + "/" + newRest
		if rewritten != match {
			changed = true
		}
		return rewritten
	})
	return []byte(result), changed, warnings
}

// runFixURLs discovers every JSON doc for collection, rewrites its embedded
// URLs, and (unless dryRun) writes changed docs back in place. When
// verifyOnly is true, no writes ever happen (dryRun is implied) and the
// function returns an error if any doc still needs a change or produced a
// warning, for use as a CI-style check.
func runFixURLs(ctx context.Context, client s3RW, bucket, collection string, concurrency int, dryRun, verifyOnly bool) error {
	if concurrency < 1 {
		concurrency = 1
	}
	if verifyOnly {
		dryRun = true
	}

	tasks, err := discoverJSONFiles(ctx, client, bucket, collection)
	if err != nil {
		return err
	}
	log.Printf("%s: %d JSON docs to check", collection, len(tasks))

	taskCh := make(chan rewriteTask)
	var checked, needsChange, rewritten, failed, warned int64
	var warnMu sync.Mutex
	var allWarnings []string

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				atomic.AddInt64(&checked, 1)
				body, contentType, err := client.GetObjectBytes(ctx, bucket, t.Key)
				if err != nil {
					atomic.AddInt64(&failed, 1)
					log.Printf("ERROR reading %s: %v", t.Key, err)
					continue
				}
				if contentType == "" {
					contentType = "application/json"
				}

				newBody, changed, warnings := rewriteURLs(body, t)
				if len(warnings) > 0 {
					atomic.AddInt64(&warned, 1)
					warnMu.Lock()
					allWarnings = append(allWarnings, warnings...)
					warnMu.Unlock()
				}
				if !changed {
					continue
				}
				atomic.AddInt64(&needsChange, 1)

				if dryRun {
					log.Printf("[dry-run] would rewrite %s", t.Key)
					continue
				}
				if err := client.PutObjectBytes(ctx, bucket, t.Key, newBody, contentType); err != nil {
					atomic.AddInt64(&failed, 1)
					log.Printf("ERROR writing %s: %v", t.Key, err)
					continue
				}
				atomic.AddInt64(&rewritten, 1)
			}
		}()
	}

	for _, t := range tasks {
		taskCh <- t
	}
	close(taskCh)
	wg.Wait()

	for _, w := range allWarnings {
		log.Printf("WARN %s", w)
	}

	log.Printf("done: %d checked, %d needed changes, %d rewritten, %d warnings, %d failures",
		checked, needsChange, rewritten, warned, failed)

	if failed > 0 {
		return fmt.Errorf("%d file operations failed", failed)
	}
	if verifyOnly && (needsChange > 0 || warned > 0) {
		return fmt.Errorf("verify failed: %d docs still need rewriting, %d warnings", needsChange, warned)
	}
	return nil
}
