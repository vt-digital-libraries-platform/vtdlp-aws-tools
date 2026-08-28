# iiif-infoFile-modifier

Finds IIIF Image API `info.json` tile-metadata files in an S3 collection,
backs each one up in place, then rewrites it to a corrected format — or, in
[rollback](#rollback) mode, reverses that and removes the backup.

## What it does

For a configured bucket, collection, and pair of DynamoDB tables, the tool:

1. **Discovers which archives belong to the collection**, via DynamoDB:
   - Looks up the Collection record in `collection_table` whose `identifier`
     attribute equals `collection_identifier`, and reads that record's `id`
     attribute.
   - Scans `archive_table` for every record whose `collection`
     attribute equals that `id`, and collects each matching record's
     `identifier` attribute.
   - For each archive identifier, lists every `info.json` object at the
     **default per-archive tile layout**:

     ```
     <collection_prefix>/<collection_identifier>/<tiles_dir_name>/<archive_identifier>-<index>/info.json
     ```

     An archive identifier may have more than one matching `-<index>`
     subdirectory, or none (e.g. tiles not yet generated). Only objects
     exactly one directory level below
     `<collection_prefix>/<collection_identifier>/<tiles_dir_name>/`, under a
     subdirectory named for an archive identifier found via DynamoDB, are
     considered. Other `info.json` files required elsewhere by the IIIF
     spec (e.g. presentation manifests), or belonging to archives not
     tracked as part of this collection, are never touched.

2. **Skips any record that doesn't look like IIIF.** Some collections are
   built from other media entirely (e.g. video), and may have unrelated
   files literally named `info.json` at the expected path — potentially
   mixed in with genuine IIIF records within the same collection, since
   different archives in one collection aren't guaranteed to share a
   media type. Every discovered `info.json` is individually checked
   (`looksLikeIIIFInfoJSON`) for the `@context`/`protocol` fields every
   IIIF `tiles/` info.json declares before anything else happens to it —
   in normal mode as part of downloading and classifying it (step 3
   below), in `-rollback` before its backup is ever looked for. A record
   that fails this check is skipped — logged, not counted as a failure,
   nothing about it modified — and the rest of the collection is
   processed as usual.

3. **Skips anything already in the corrected format.** Each matched
   `info.json` is downloaded and checked (`isAlreadyTransformed`): if its
   `profile` already declares `formats`/`qualities` — the shape only this
   tool's own output ever has — it's left alone entirely, with no backup
   and no rewrite. This makes it safe to run the tool again over a
   collection it already processed: without this check, a second run would
   back up the already-corrected file over the real original, permanently
   losing data (like `sizes`) that only the true original had.

4. **Backs up** every remaining (still-original-format) `info.json`, as a
   server-side S3 copy named `backup_info.json` (configurable) at the same
   key location. If any backup fails, that collection's modification step
   is aborted (nothing is rewritten for it), without stopping other
   collections when running with `-all`.

5. **Rewrites** each backed-up `info.json` in place (same bucket, same key)
   to the corrected format — see [Transform](#transform) below.

Run with `-dry-run` (or `dry_run: true` in the config) to see exactly what
would be backed up and rewritten without touching S3.

Run with `-rollback` to reverse this instead — see [Rollback](#rollback).

Run with `-all` to process every visible collection instead of one — see
[Processing every collection](#processing-every-collection).

## Transform

Given an original `info.json` like [`incorrect_info.json`](incorrect_info.json):

```json
{
  "@context": "http://iiif.io/api/image/2/context.json",
  "@id": "https://<cloudfront-distribution-id>.cloudfront.net/federated/glink/tiles/glink002121-1",
  "protocol": "http://iiif.io/api/image",
  "width": 4309,
  "height": 2702,
  "sizes": [ ... ],
  "profile": [
    "http://iiif.io/api/image/2/level0.json",
    { "supports": ["cors", "sizeByWhListed", "baseUriRedirect"] }
  ],
  "tiles": [ { "width": 512, "scaleFactors": [1, 2, 4, 8] } ]
}
```

the tool writes back [`corrected_info.json`](corrected_info.json):

```json
{
  "@context": "http://iiif.io/api/image/2/context.json",
  "@id": "https://<cloudfront-distribution-id>.cloudfront.net/federated/glink/tiles/glink002121-1",
  "profile": [
    "http://iiif.io/api/image/2/level0.json",
    {
      "formats": ["jpg"],
      "qualities": ["default"],
      "supports": ["cors", "baseUriRedirect"]
    }
  ],
  "protocol": "http://iiif.io/api/image",
  "tiles": [ { "scaleFactors": [1, 2, 4, 8], "width": 512 } ],
  "width": 4309,
  "height": 2702
}
```

Specifically (see `transformInfoJSON` in [`main.go`](main.go)):

| Field | Behavior |
|---|---|
| `@id`, `width`, `height` | Preserved from the input. |
| `tiles[]` | Preserved from the input, but each entry's fields are reordered to `scaleFactors` then `width`. |
| `sizes` | Dropped entirely. |
| `@context` | Hardcoded to `http://iiif.io/api/image/2/context.json`. |
| `protocol` | Hardcoded to `http://iiif.io/api/image`. |
| `profile` | Hardcoded to level0 + `{"formats":["jpg"],"qualities":["default"],"supports":["cors","baseUriRedirect"]}`, regardless of what the input had. |

`@context`, `protocol`, and `profile` are hardcoded (not derived from the
input) because every file this tool processes comes from the same tiler,
which always produces this exact shape. Re-running the transform on an
already-corrected file is a no-op (see `TestTransformInfoJSON_Idempotent` in
[`main_test.go`](main_test.go)) — it's safe to run more than once.

If you need to change the target values (e.g. a different tiler that emits
PNG, or additional `supports` features), edit the `requiredContext`,
`requiredProtocol`, and `requiredProfile` values at the top of `main.go`.

## Rollback

Run with `-rollback` to restore every discovered `info.json` from its
`backup_info.json`: for each key, `info.json` itself is checked
(`looksLikeIIIFInfoJSON`) — a record that never looked like IIIF was
never backed up by a forward run, so it's skipped without ever looking
for its backup. Otherwise the backup is checked (`isPreTransformShape`)
to confirm it's actually a pre-transform object — it has a non-empty
`sizes` array and its `profile` doesn't declare `formats` — and if so,
it's copied over the `info.json` key (a server-side S3 copy, the same
"move" idiom `aws s3 mv` uses under the hood) and then deleted. This
restores the true original bytes, rather than reconstructing an
approximation of them from hardcoded field values.

Discovery works exactly the same as a normal run (DynamoDB Collection →
Archive → S3 key list) — rollback just uses a different per-key action.
**A `backup_info.json` must exist for every IIIF-shaped key being rolled
back** — if one is missing, or doesn't pass the pre-transform shape
check, that key fails and is counted in the summary's `failed` total; no
backups are deleted for keys that fail.

```sh
# Dry run first:
./iiif-infoFile-modifier -config config.yaml -rollback -dry-run

# Live rollback:
./iiif-infoFile-modifier -config config.yaml -rollback
```

See `runRollback` and `isPreTransformShape` in [`main.go`](main.go).
`TestIsPreTransformShape` in [`main_test.go`](main_test.go) verifies the
shape check accepts `incorrect_info.json` (pre-transform) and rejects
`corrected_info.json` (post-transform).

## Processing every collection

Run with `-all` to ignore `collection_identifier` and instead process
every Collection record in `collection_table` whose `visible` attribute is
`true`: for each one (identified by its own `identifier` and `id`
attributes), the tool runs the same discovery → skip/backup/transform (or
`-rollback`) flow described above, using that collection's `identifier` as
the S3 path segment (`collection_prefix` + `/` + `identifier` + `/`).

Collections are processed one after another (each collection's own
per-key work is still concurrent, per [Concurrency](#concurrency) below).
A failure discovering or processing one collection is logged and counted,
but doesn't stop the rest from being attempted — the process exits
non-zero at the end if any collection had a failure. With more than one
collection found, a final `all done: collections=N total_failed=N` line
is logged after every collection has been attempted.

```sh
# Dry run first:
./iiif-infoFile-modifier -config config.yaml -all -dry-run

# Live, every visible collection:
./iiif-infoFile-modifier -config config.yaml -all

# Roll back every visible collection:
./iiif-infoFile-modifier -config config.yaml -all -rollback
```

See `findVisibleCollections` in [`main.go`](main.go).

## Configuration

Config is a YAML file, passed with `-config` (defaults to `config.yaml` in
the working directory). See [`config.yaml`](config.yaml) for a full example
with comments.

| Key | Required | Default | Description |
|---|---|---|---|
| `region` | yes | — | AWS region the bucket and DynamoDB tables live in. |
| `bucket` | yes | — | S3 bucket containing the info.json files. |
| `collection_prefix` | yes | — | Key prefix *above* the collection root (trailing slash optional; added automatically). `collection_identifier` supplies the final path segment. |
| `collection_table` | yes | — | DynamoDB table holding Collection records; looked up by `identifier` to find the collection's `id`. |
| `archive_table` | yes | — | DynamoDB table holding Archive records; scanned for `collection` matching the collection's `id` to find archive identifiers. |
| `collection_identifier` | yes, unless `-all` | — | Value matched against `collection_table`'s `identifier` attribute. Also the final path segment of the S3 collection root: `collection_prefix` + `/` + `collection_identifier` + `/`. Ignored when `-all` is passed. |
| `tiles_dir_name` | no | `tiles` | Directory directly under the collection root (`collection_prefix`/`collection_identifier`) holding one subdirectory per archive's tiles. |
| `info_file_name` | no | `info.json` | Filename to look for inside each archive's tile directory. |
| `backup_file_name` | no | `backup_info.json` | Filename for the pre-modification backup, written alongside each `info.json`. |
| `concurrency` | no | `10` | How many S3/DynamoDB requests run in parallel — both per-archive object listing, and per-key download/backup/transform/upload (or rollback). |
| `dry_run` | no | `false` | If true, scan and log planned actions but write nothing to S3. |

### Concurrency

Per-key work (downloading, backing up, transforming, uploading, or rolling
back an `info.json`) is independent across keys, so it runs concurrently, up
to `concurrency` requests in flight at once (default 10). Discovery's
per-archive S3 listing (`findInfoObjects`) is parallelized the same way.
DynamoDB discovery itself (`findCollectionID`, `findArchiveIdentifiers`) is
inherently sequential per table — each Scan page must be fetched after the
last — so it isn't affected by this setting. Raise `concurrency` for a large
collection on a fast connection, or lower it if you're hitting request
throttling.

## Usage

```sh
go build -o iiif-infoFile-modifier .

# Dry run first — always do this before a live run:
./iiif-infoFile-modifier -config config.yaml -dry-run

# Live run:
./iiif-infoFile-modifier -config config.yaml
```

Flags:

- `-config <path>` — path to the YAML config file (default `config.yaml`).
- `-dry-run` — force dry-run mode even if `dry_run: false` in the config.
  (`dry_run: true` in the config cannot be overridden back to live via
  flags — edit the file instead.)
- `-rollback` — reverse mode; see [Rollback](#rollback).
- `-all` — process every visible collection instead of one; see
  [Processing every collection](#processing-every-collection).

The process logs a summary line per collection. Normal mode:

```
done: found=42 skipped=0 non_iiif=0 backed_up=42 modified=42 failed=0
```

Rollback mode:

```
done: found=42 rolled_back=42 non_iiif=0 failed=0
```

`non_iiif` counts keys skipped because they didn't look like an IIIF
info.json (see [What it does](#what-it-does), step 2) — never a failure.

With `-all` and more than one collection found, a final line follows once
every collection has been attempted:

```
all done: collections=5 total_failed=0
```

and the process exits non-zero if any object failed to back up/download/
transform/upload (normal mode) or download/rollback/upload/delete
(rollback mode), in any collection processed.

## AWS credentials & permissions

The tool uses the AWS SDK's default credential chain (environment
variables, shared config/credentials file, SSO, instance/task role, etc.) —
there is no credentials configuration in `config.yaml`. Whatever identity
runs the tool needs, at minimum:

On the target bucket/prefix:
- `s3:ListBucket`
- `s3:GetObject`
- `s3:PutObject`
- `s3:CopyObject` (used for the backup step, and to restore from backup during `-rollback`)
- `s3:DeleteObject` (used to remove `backup_info.json` during `-rollback`)

On `collection_table` and `archive_table`:
- `dynamodb:Scan`

## Development

```sh
go build ./...
go vet ./...
gofmt -l .        # should print nothing
go test ./...     # runs the transform against incorrect_info.json / corrected_info.json
```

`main_test.go` verifies the transform against the two fixture files in this
directory, checks idempotency, checks that `isAlreadyTransformed` and
`isPreTransformShape` correctly distinguish the two fixtures, and checks
that `looksLikeIIIFInfoJSON` accepts both fixtures but rejects a
non-IIIF-shaped JSON blob; there's no S3/DynamoDB interaction in the test
suite, so no AWS credentials are needed
to run it.
