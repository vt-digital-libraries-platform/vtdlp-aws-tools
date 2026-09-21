// iawa-migrate discovers "item" directories nested inside legacy
// sub-collection directories (Box/Folder/etc, at any depth, under any
// naming scheme) in the IAWA IIIF S3 layout, and relocates them directly
// under their collection root - matching the layout already used by
// collections that have been migrated.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
)

const defaultBucket = "vtdlp-pprd.img.cloud.lib.vt.edu"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "discover":
		runDiscover(args)
	case "migrate":
		runMigrate(args)
	case "verify":
		runVerify(args)
	case "fix-urls":
		runFixURLsCmd(args)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `iawa-migrate <command> [flags]

Commands:
  discover  -collection NAME [-bucket NAME]
      List every nested item directory that needs to move to the
      collection root, and print warnings (collisions, missing tiles, etc).

  migrate   -collection NAME [-bucket NAME] [-concurrency N] [-dry-run] [-delete]
      Discover, then execute the move: copies item files (rewriting embedded
      URLs) and tile files to the collection root. Without -delete, old
      objects are left in place after a successful copy (safe re-run: use
      -dry-run first, then run for real, then run again with -delete once
      you've spot-checked the results).

  verify    -collection NAME [-bucket NAME]
      Re-run discovery and confirm zero items remain nested below the
      collection root.

  fix-urls  -collection NAME [-bucket NAME] [-concurrency N] [-dry-run] [-verify]
      Rewrite embedded asset URLs (manifest.json, info.json, canvas/*.json,
      annotation/*.json, sequence/*.json) in place: swaps the stale
      img.cloud.lib.vt.edu domain for the correct CloudFront host and drops
      any leftover sub-collection path segments, using each file's own
      current (collection-root) key as ground truth. Idempotent - safe to
      re-run. -dry-run logs planned rewrites without writing. -verify is a
      read-only check that exits non-zero if any doc still needs a rewrite.
`)
}

func runDiscover(args []string) {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	bucket := fs.String("bucket", defaultBucket, "S3 bucket")
	collection := fs.String("collection", "", "collection directory name, e.g. Ms1990_025_Rudoff")
	fs.Parse(args)
	if *collection == "" {
		log.Fatal("-collection is required")
	}

	ctx := context.Background()
	client, err := newS3(ctx)
	if err != nil {
		log.Fatal(err)
	}

	plan, err := buildPlan(ctx, client, *bucket, *collection)
	if err != nil {
		log.Fatal(err)
	}

	printPlan(plan)
}

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	bucket := fs.String("bucket", defaultBucket, "S3 bucket")
	collection := fs.String("collection", "", "collection directory name, e.g. Ms1990_025_Rudoff")
	concurrency := fs.Int("concurrency", 16, "number of concurrent worker goroutines")
	dryRun := fs.Bool("dry-run", false, "print planned actions without writing to S3")
	del := fs.Bool("delete", false, "delete old objects after a successful copy for each item")
	fs.Parse(args)
	if *collection == "" {
		log.Fatal("-collection is required")
	}

	ctx := context.Background()
	client, err := newS3(ctx)
	if err != nil {
		log.Fatal(err)
	}

	plan, err := buildPlan(ctx, client, *bucket, *collection)
	if err != nil {
		log.Fatal(err)
	}
	printPlan(plan)

	if len(plan.Warnings) > 0 && !*dryRun {
		log.Fatal("refusing to run a live migration while warnings are present; resolve them or run discover to inspect, or pass -dry-run to preview anyway")
	}
	if len(plan.Items) == 0 {
		log.Println("nothing to migrate")
		return
	}

	if err := runMigration(ctx, client, plan, *concurrency, *dryRun, *del); err != nil {
		log.Fatal(err)
	}
}

func runVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	bucket := fs.String("bucket", defaultBucket, "S3 bucket")
	collection := fs.String("collection", "", "collection directory name, e.g. Ms1990_025_Rudoff")
	fs.Parse(args)
	if *collection == "" {
		log.Fatal("-collection is required")
	}

	ctx := context.Background()
	client, err := newS3(ctx)
	if err != nil {
		log.Fatal(err)
	}

	plan, err := buildPlan(ctx, client, *bucket, *collection)
	if err != nil {
		log.Fatal(err)
	}

	if len(plan.Items) == 0 && len(plan.Warnings) == 0 {
		fmt.Printf("OK: %s has no nested items remaining\n", *collection)
		return
	}
	printPlan(plan)
	os.Exit(1)
}

func runFixURLsCmd(args []string) {
	fs := flag.NewFlagSet("fix-urls", flag.ExitOnError)
	bucket := fs.String("bucket", defaultBucket, "S3 bucket")
	collection := fs.String("collection", "", "collection directory name, e.g. Ms1990_025_Rudoff")
	concurrency := fs.Int("concurrency", 16, "number of concurrent worker goroutines")
	dryRun := fs.Bool("dry-run", false, "log planned rewrites without writing to S3")
	verify := fs.Bool("verify", false, "read-only check: exit non-zero if any doc still needs a rewrite")
	fs.Parse(args)
	if *collection == "" {
		log.Fatal("-collection is required")
	}

	ctx := context.Background()
	client, err := newS3(ctx)
	if err != nil {
		log.Fatal(err)
	}

	if err := runFixURLs(ctx, client, *bucket, *collection, *concurrency, *dryRun, *verify); err != nil {
		if *verify {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		log.Fatal(err)
	}
	if *verify {
		fmt.Printf("OK: %s has no remaining stale URLs\n", *collection)
	}
}

func printPlan(plan *Plan) {
	fmt.Printf("collection: %s (bucket: %s)\n", plan.Collection, plan.Bucket)
	fmt.Printf("items to move: %d\n", len(plan.Items))
	for _, item := range plan.Items {
		if item.AlreadyAtRoot {
			fmt.Printf("  %s [already at root - cleanup only]\n    stale copy: %s\n    root copy:  %s\n    files: %d, tile files: %d (all pending delete only)\n",
				item.ItemName, item.OldItemDir, item.NewItemDir, len(item.Files), len(item.TileFiles))
			continue
		}
		fmt.Printf("  %s\n    from: %s\n    to:   %s\n    files: %d, tile files: %d\n",
			item.ItemName, item.OldItemDir, item.NewItemDir, len(item.Files), len(item.TileFiles))
	}
	if len(plan.Warnings) > 0 {
		fmt.Printf("warnings: %d\n", len(plan.Warnings))
		for _, w := range plan.Warnings {
			fmt.Printf("  ! %s\n", w)
		}
	}
}
