package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// task is one unit of work: move a single object, optionally rewriting its
// content, as part of migrating a given item.
type task struct {
	itemIdx int
	file    FileTask
}

// itemResult tracks how a single item's migration went so we know whether
// it is safe to delete its old keys.
type itemResult struct {
	mu     sync.Mutex
	failed bool
	errs   []error
}

func (r *itemResult) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
	r.errs = append(r.errs, err)
}

// runMigration executes plan with a bounded pool of goroutines. When dryRun
// is true, no S3 writes happen — every planned action is only logged. When
// del is true, an item's old objects are deleted once every new object for
// that item has been written successfully.
func runMigration(ctx context.Context, client s3RW, plan *Plan, concurrency int, dryRun, del bool) error {
	if concurrency < 1 {
		concurrency = 1
	}

	tasks := make(chan task)
	results := make([]*itemResult, len(plan.Items))
	for i := range results {
		results[i] = &itemResult{}
	}

	var copied, rewritten, failed int64

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				var err error
				if dryRun {
					verb := "copy"
					if t.file.Rewrite {
						verb = "rewrite+upload"
					}
					log.Printf("[dry-run] %s %s -> %s", verb, t.file.OldKey, t.file.NewKey)
				} else if t.file.Rewrite {
					err = rewriteAndUpload(ctx, client, plan.Bucket, plan.CollectionRoot, plan.Items[t.itemIdx], t.file)
					if err == nil {
						atomic.AddInt64(&rewritten, 1)
					}
				} else {
					err = client.CopyObject(ctx, plan.Bucket, t.file.OldKey, t.file.NewKey)
					if err == nil {
						atomic.AddInt64(&copied, 1)
					}
				}
				if err != nil {
					atomic.AddInt64(&failed, 1)
					results[t.itemIdx].fail(err)
					log.Printf("ERROR %s -> %s: %v", t.file.OldKey, t.file.NewKey, err)
				}
			}
		}()
	}

	for i, item := range plan.Items {
		if item.AlreadyAtRoot {
			// Nothing to copy or rewrite - the target already exists.
			// Files/TileFiles here only carry OldKey, for the delete pass below.
			if dryRun {
				log.Printf("[dry-run] %q already exists at root; nested copy at %s is cleanup-only (%d files, %d tile files)",
					item.ItemName, item.OldItemDir, len(item.Files), len(item.TileFiles))
			}
			continue
		}
		for _, f := range item.Files {
			tasks <- task{itemIdx: i, file: f}
		}
		for _, f := range item.TileFiles {
			tasks <- task{itemIdx: i, file: f}
		}
	}
	close(tasks)
	wg.Wait()

	log.Printf("done: %d files copied, %d files rewritten, %d failures", copied, rewritten, failed)

	if del {
		for i, item := range plan.Items {
			if !dryRun && results[i].failed {
				log.Printf("skipping delete of old keys for %q: migration had errors", item.ItemName)
				continue
			}
			var oldKeys []string
			for _, f := range item.Files {
				oldKeys = append(oldKeys, f.OldKey)
			}
			for _, f := range item.TileFiles {
				oldKeys = append(oldKeys, f.OldKey)
			}
			if dryRun {
				log.Printf("[dry-run] would delete %d old keys for %q", len(oldKeys), item.ItemName)
				continue
			}
			if err := client.DeleteObjects(ctx, plan.Bucket, oldKeys); err != nil {
				log.Printf("ERROR deleting old keys for %q: %v", item.ItemName, err)
				continue
			}
			log.Printf("deleted %d old keys for %q", len(oldKeys), item.ItemName)
		}
	}

	if dryRun {
		return nil
	}

	if failed > 0 {
		return fmt.Errorf("%d file operations failed", failed)
	}
	return nil
}

// rewriteAndUpload downloads a JSON file, replaces every occurrence of the
// item's old sub-collection path with the collection root, and uploads the
// result to the new key.
func rewriteAndUpload(ctx context.Context, client s3RW, bucket, collectionRoot string, item ItemMove, f FileTask) error {
	body, contentType, err := client.GetObjectBytes(ctx, bucket, f.OldKey)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	rewritten := strings.ReplaceAll(string(body), item.OldSubPath, collectionRoot)
	return client.PutObjectBytes(ctx, bucket, f.NewKey, []byte(rewritten), contentType)
}
