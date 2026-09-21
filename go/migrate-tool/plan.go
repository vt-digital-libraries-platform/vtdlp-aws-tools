package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// FileTask describes moving one object from OldKey to NewKey.
// Rewrite is true when the object's content contains embedded URLs that
// need the old sub-collection path segment replaced with the new one.
type FileTask struct {
	OldKey  string
	NewKey  string
	Rewrite bool
}

// ItemMove describes relocating one "item" directory (and its associated
// tile-pyramid files) from a nested sub-collection up to the collection root.
//
// AlreadyAtRoot is true when a copy of this item already exists at the
// collection root (e.g. a previous `migrate` run without -delete already
// copied it there). In that case Files/TileFiles list the OLD, still-nested
// keys only, for deletion — there is nothing left to copy or rewrite.
type ItemMove struct {
	ItemName      string
	OldItemDir    string // e.g. iawa/Coll/Sub1/Sub2/Item/
	NewItemDir    string // e.g. iawa/Coll/Item/
	OldSubPath    string // e.g. iawa/Coll/Sub1/Sub2/  (the dir directly containing OldItemDir; also houses this level's local tiles/)
	AlreadyAtRoot bool
	Files         []FileTask
	TileFiles     []FileTask
}

// Plan is the full set of moves discovered for one collection.
type Plan struct {
	Bucket         string
	Collection     string
	CollectionRoot string // e.g. iawa/Coll/
	Items          []ItemMove
	Warnings       []string
}

func dirOf(key string) string {
	// key looks like ".../name/manifest.json" or ".../name/tiles/x-1/full/..."
	d := path.Dir(key)
	if d == "." {
		return ""
	}
	return d + "/"
}

// suffixSet returns, for every key with the given prefix, the part of the
// key after that prefix — used to compare "does this item's file set here
// match its file set there" without caring about the path prefix itself.
func suffixSet(keySet map[string]struct{}, prefix string) map[string]struct{} {
	out := make(map[string]struct{})
	for key := range keySet {
		if strings.HasPrefix(key, prefix) {
			out[strings.TrimPrefix(key, prefix)] = struct{}{}
		}
	}
	return out
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// buildPlan discovers every item directory nested below the collection root
// (at any depth, under any sub-collection naming scheme) and builds the
// move plan to relocate each one directly under the collection root.
func buildPlan(ctx context.Context, client s3Lister, bucket, collection string) (*Plan, error) {
	collectionRoot := fmt.Sprintf("iawa/%s/", collection)

	objs, err := client.ListAllKeys(ctx, bucket, collectionRoot)
	if err != nil {
		return nil, err
	}

	keySet := make(map[string]struct{}, len(objs))
	for _, o := range objs {
		keySet[o.Key] = struct{}{}
	}

	plan := &Plan{Bucket: bucket, Collection: collection, CollectionRoot: collectionRoot}

	// Find every directory that is an "item": it directly contains manifest.json.
	var itemDirs []string
	for key := range keySet {
		if strings.HasSuffix(key, "/manifest.json") {
			itemDirs = append(itemDirs, dirOf(key))
		}
	}
	sort.Strings(itemDirs)

	// Pass 1: register every item that already sits directly at the
	// collection root (pre-existing, or landed there by a prior migrate
	// run). Do this before looking at nested items so processing order
	// doesn't matter.
	rootItemDirs := make(map[string]string) // itemName -> root item dir
	for _, itemDir := range itemDirs {
		rel := strings.TrimSuffix(strings.TrimPrefix(itemDir, collectionRoot), "/")
		parts := strings.Split(rel, "/")
		if len(parts) != 1 {
			continue
		}
		itemName := parts[0]
		if prev, ok := rootItemDirs[itemName]; ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("duplicate root item name %q: %s and %s", itemName, prev, itemDir))
			continue
		}
		rootItemDirs[itemName] = itemDir
	}

	// Pass 2: process nested items.
	seenNested := make(map[string]string) // itemName -> nested dir, to catch nested-vs-nested collisions
	for _, itemDir := range itemDirs {
		rel := strings.TrimSuffix(strings.TrimPrefix(itemDir, collectionRoot), "/")
		parts := strings.Split(rel, "/")
		if len(parts) == 1 {
			continue // already handled in pass 1
		}
		itemName := parts[len(parts)-1]

		subPath := strings.Join(parts[:len(parts)-1], "/") + "/" // e.g. Sub1/Sub2/
		oldSubPath := collectionRoot + subPath                   // Q, absolute
		tilesPrefix := oldSubPath + "tiles/"
		itemTilePrefix := tilesPrefix + itemName + "-"

		if rootDir, ok := rootItemDirs[itemName]; ok {
			// This item already has a copy sitting at the collection root.
			// That's expected after a migrate run without -delete - treat
			// the nested copy as a cleanup-only candidate, not a collision,
			// but only if its file set actually matches what's at root
			// (a real, unrelated naming collision should still be flagged).
			oldFileSuffixes := suffixSet(keySet, itemDir)
			rootFileSuffixes := suffixSet(keySet, rootDir)
			oldTileSuffixes := suffixSet(keySet, itemTilePrefix)
			rootTileSuffixes := suffixSet(keySet, collectionRoot+"tiles/"+itemName+"-")

			if !sameSet(oldFileSuffixes, rootFileSuffixes) || !sameSet(oldTileSuffixes, rootTileSuffixes) {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"item %q already exists at root (%s) but its file set differs from the nested copy at %s; skipping automatic cleanup - investigate manually",
					itemName, rootDir, itemDir))
				continue
			}

			move := ItemMove{
				ItemName:      itemName,
				OldItemDir:    itemDir,
				NewItemDir:    rootDir,
				OldSubPath:    oldSubPath,
				AlreadyAtRoot: true,
			}
			for key := range keySet {
				if strings.HasPrefix(key, itemDir) {
					move.Files = append(move.Files, FileTask{OldKey: key})
				}
				if strings.HasPrefix(key, itemTilePrefix) {
					move.TileFiles = append(move.TileFiles, FileTask{OldKey: key})
				}
			}
			sort.Slice(move.Files, func(i, j int) bool { return move.Files[i].OldKey < move.Files[j].OldKey })
			sort.Slice(move.TileFiles, func(i, j int) bool { return move.TileFiles[i].OldKey < move.TileFiles[j].OldKey })
			plan.Items = append(plan.Items, move)
			continue
		}

		if prev, ok := seenNested[itemName]; ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("collision: item %q at %s and %s would both move to the same root path", itemName, prev, itemDir))
			continue
		}
		seenNested[itemName] = itemDir

		newItemDir := collectionRoot + itemName + "/"

		move := ItemMove{
			ItemName:   itemName,
			OldItemDir: itemDir,
			NewItemDir: newItemDir,
			OldSubPath: oldSubPath,
		}

		// Files belonging to the item itself (manifest.json, canvas/*, annotation/*, sequence/*).
		for key := range keySet {
			if strings.HasPrefix(key, itemDir) {
				suffix := strings.TrimPrefix(key, itemDir)
				newKey := newItemDir + suffix
				move.Files = append(move.Files, FileTask{
					OldKey:  key,
					NewKey:  newKey,
					Rewrite: strings.HasSuffix(key, ".json"),
				})
			}
		}
		sort.Slice(move.Files, func(i, j int) bool { return move.Files[i].OldKey < move.Files[j].OldKey })

		// Tile files for this item live as siblings under this nesting
		// level's own tiles/ dir, named "<ItemName>-<page>/...".
		newTilesPrefix := collectionRoot + "tiles/"
		for key := range keySet {
			if strings.HasPrefix(key, itemTilePrefix) {
				suffix := strings.TrimPrefix(key, tilesPrefix) // "<ItemName>-N/..."
				newKey := newTilesPrefix + suffix
				move.TileFiles = append(move.TileFiles, FileTask{
					OldKey:  key,
					NewKey:  newKey,
					Rewrite: false,
				})
			}
		}
		sort.Slice(move.TileFiles, func(i, j int) bool { return move.TileFiles[i].OldKey < move.TileFiles[j].OldKey })

		if len(move.TileFiles) == 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("item %q at %s has no matching tile files under %s", itemName, itemDir, itemTilePrefix))
		}

		plan.Items = append(plan.Items, move)
	}

	sort.Slice(plan.Items, func(i, j int) bool { return plan.Items[i].OldItemDir < plan.Items[j].OldItemDir })

	return plan, nil
}
