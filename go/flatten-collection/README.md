# flatten-collection

Flattens a collection's media backup, and expands it back.

A backup rooted at `root_path` is shaped like:

```
<root>/<identifier>/Access/<file>.tif
```

Two subcommands:

- `flatten` — moves every matching file up to `<root>/<file>.tif`, then
  removes each `Access/` and `<identifier>/` directory left empty. Top-level
  directories with no `Access/` (e.g. `targets/`, `ingest_results/`) and
  non-matching files are left alone. Records each move in a manifest file in
  the root.
- `expand` — the reverse: moves each matching root file back into
  `<identifier>/Access/`. Files listed in the manifest go back to their
  exact original path; others get their `<identifier>` from
  `identifier_pattern`. Restored entries are dropped from the manifest, and
  the manifest is deleted once it's empty.

Files are moved with `os.Rename` (no copying) by a pool of `concurrency`
goroutines. Neither command overwrites an existing file: a name collision is
reported as an error and that file is left where it was.

## Build

```
go build -o flatten-collection .
```

## Config

Both commands take `-config config.yaml` (default `config.yaml`):

```yaml
# Substituted for {collection_identifier} in root_path.
collection_identifier: glink

root_path: ~/dev/dlp/assets/{collection_identifier}/media

subdir: Access          # default Access
extensions: [.tif]      # case-insensitive; default [.tif]
concurrency: 16         # default 16
manifest_file: flatten_manifest.json

# expand only, for files not in the manifest: matched against the file name
# without its extension; capture group 1 is the identifier.
# glink001001003 -> glink001001. Empty means the whole name is the identifier.
identifier_pattern: '^(.+)\d{3}$'
```

## Usage

```
flatten-collection flatten [-config config.yaml] [-collection_identifier ID] [-dry_run] [-v]
flatten-collection expand  [-config config.yaml] [-collection_identifier ID] [-dry_run] [-v]
```

- `-collection_identifier` overrides the config value, so one config serves
  every collection.
- `-dry_run` checks every move (including collisions) without changing
  anything.
- `-v` prints each file moved and directory removed.

The process exits non-zero if any file could not be moved.

## Test

```
go test ./...
```
