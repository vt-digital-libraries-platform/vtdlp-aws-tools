# archive-title-dedup

Finds Archive records that share an exact title, and disambiguates them by
appending a suffix and the record's identifier to the title.

Three subcommands:

- `report` — scans the Archive table and writes a JSON report of duplicate
  titles.
- `apply` — reads a report (or change-log) and rewrites titles for one
  collection's records, writing a change-log of what it did.
- `rollback` — reads a report or change-log and restores the recorded
  original titles.
- `revert-legacy` — restores titles disambiguated by the pre-`apply`
  version of this tool, for a collection with no report or change-log to
  roll back from.

## Build

```
go build -o archive-title-dedup .
```

## Config

All commands take `-config config.yaml` (default `config.yaml`):

```yaml
region: us-east-1

# Archive table to scan for records with non-unique titles.
table_name: Archive-bxbkjhe235e3jcwcjcji5txvlm-vtdlpdev

# Collection table, used by `report` to resolve each record's parent
# collection id to the collection's identifier. Required for `report`.
collection_table_name: Collection-bxbkjhe235e3jcwcjcji5txvlm-vtdlpdev

# Directory the report/change-log is written to (created if missing).
output_dir: output

# Report file name inside output_dir. Defaults to duplicate_titles.json.
output_file: duplicate_titles.json

# Number of parallel scan segments (report) / parallel writers (apply, rollback).
concurrency: 10

# Optional fallback defaults for apply's -collection_identifier/-suffix
# flags, used only when the flag is omitted.
collection_identifier: FCHS_OBJ
suffix: Map
```

## Report shape

`report` writes (and `apply`/`rollback` read) a JSON document shaped like:

```json
{
  "collection_identifier": "FCHS_OBJ",
  "timestamp": "20260922T000000Z",
  "duplicates": [
    {
      "title": "1957 Coeburn Quadrangle Virginia",
      "records": [
        {
          "id": "01f2...",
          "identifier": "nmcst005196",
          "collection_id": "coll-a-id",
          "collection_identifier": "FCHS_OBJ"
        },
        {
          "id": "01f3...",
          "identifier": "nmcst005197",
          "collection_id": "coll-a-id",
          "collection_identifier": "FCHS_OBJ"
        }
      ]
    }
  ]
}
```

`collection_id` is the record's parent collection's DynamoDB key
(`parent_collection[0]` on the Archive item). `collection_identifier` is
that collection's human-readable identifier, resolved by `report` from
`collection_table_name`. A record with no parent collection has both fields
`null`.

`collection_identifier` and `timestamp` at the top level are only populated
in a change-log written by `apply` (an original `report` omits them).

## Usage

### report

```
archive-title-dedup report -config config.yaml
```

Scans the whole table and writes
`<output_dir>/<output_file stem>_<timestamp><ext>` (default
`output/duplicate_titles_20260922T153000Z.json`) containing every title
shared by two or more records. `<timestamp>` is `YYYYMMDDTHHMMSSZ` (UTC),
so successive runs never overwrite each other's report.

Alongside it, `report` writes a Markdown file with the same name but a `.md`
extension (default `output/duplicate_titles_20260922T153000Z.md`) listing
the identifier of every collection that has at least one record with a
duplicate title. Those are the collections you can pass to `apply
-collection_identifier`. Records whose collection could not be resolved are
not included.

### apply

```
archive-title-dedup apply -config config.yaml \
  -input output/duplicate_titles_20260922T153000Z.json \
  -collection_identifier FCHS_OBJ \
  -suffix Map \
  [-dry-run]
```

For every record in `-input` whose `collection_identifier` matches
`-collection_identifier`, rewrites its title to:

```
<original title> - <suffix>: <record identifier>
```

Records belonging to other collections — including other members of a
title-group that spans multiple collections — are left untouched.
`-collection_identifier` and `-suffix` fall back to `collection_identifier`
and `suffix` in `config.yaml` when omitted. `-dry-run` logs the planned
changes without writing to DynamoDB.

On success, writes a change-log to
`<output_dir>/changelog_<collection_identifier>_<timestamp>.json`, in the
same shape as a report, with each written record's `new_title` filled in.
Keep this file — it's what `rollback` uses to undo the change.

### rollback

```
archive-title-dedup rollback -config config.yaml \
  -collection_identifier FCHS_OBJ \
  [-dry-run] <report-or-changelog.json>
```

For every record in the given file whose `collection_identifier` matches
`-collection_identifier`, restores its title to the recorded original
(`duplicates[].title`), regardless of the record's current live value in
DynamoDB. Records belonging to other collections are left untouched, just
like `apply`. Accepts either an original `report` file or a change-log
written by `apply` — both share the same JSON shape.
`-collection_identifier` falls back to `collection_identifier` in
`config.yaml` when omitted. `-dry-run` logs the planned changes without
writing to DynamoDB.

### revert-legacy

```
archive-title-dedup revert-legacy -config config.yaml \
  -collection_identifier vtec \
  -suffix ": Specimen" \
  [-dry-run]
```

For a collection that was disambiguated by an older version of this tool
(before `apply`/`rollback` existed, when titles were rewritten to
`<original title><suffix>-<index>` with no report or change-log written),
scans the live table for records in `-collection_identifier` whose title
still ends in `<suffix>-<digits>`, and restores each to the title with
that suffix and index stripped off. `-collection_identifier` and `-suffix`
fall back to `collection_identifier` and `suffix` in `config.yaml` when
omitted. `-dry-run` logs the planned changes without writing to DynamoDB.

## Typical workflow

```
archive-title-dedup report -config config.yaml
# -> output/duplicate_titles_<timestamp>.json
archive-title-dedup apply -config config.yaml -input output/duplicate_titles_<timestamp>.json \
  -collection_identifier FCHS_OBJ -suffix Map -dry-run
archive-title-dedup apply -config config.yaml -input output/duplicate_titles_<timestamp>.json \
  -collection_identifier FCHS_OBJ -suffix Map
# ... if something needs undoing:
archive-title-dedup rollback -config config.yaml -collection_identifier FCHS_OBJ \
  output/changelog_FCHS_OBJ_<timestamp>.json
```
