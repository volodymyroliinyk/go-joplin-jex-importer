# Joplin JEX Importer (Go)

Import Joplin `.jex` exports into **Joplin Desktop** via the Data API — into a chosen notebook — with **idempotent**
upserts (no duplicates), **resource** handling, and **robust tag linking**.

> Works with modern JEX where item properties are stored at the end of each `.md` file as `key: value` lines.

## Features

- **Upsert notes by ID**: if a note with the same `id` exists — it’s **updated**, not duplicated.
- **Import resources**: uploads referenced resources and rewrites resource IDs in note bodies if Joplin reassigns them.
- **Smart tags**:
    - Ensures tags **by title** (searches existing, creates if absent).
    - Links notes to tags using **real DB tag IDs** (not file IDs), so repeated imports don't break.
- **Note–tag links**: honors explicit `note_tag` relations from the export.
- **Timestamps**: optionally preserve `user_created_time` / `user_updated_time` from the export.
- **Target notebook**: all imported notes are assigned to the notebook you specify.
- **Idempotent re-runs**: safe to run repeatedly; no duplicate notes or tag-links.

## Requirements

- **Joplin Desktop** running locally with **Web Clipper (Data API) enabled** and a valid **token**.
    - Default API base: `http://127.0.0.1:41184`.
- Go 1.20+ (recommended latest stable).

## Quick start

```bash
go run ./main.go \
  -jex "/path/to/export.jex" \
  -notebook "TARGET_NOTEBOOK_ID" \
  -token "YOUR_JOPLIN_TOKEN" \
  -api "http://127.0.0.1:41184" \
  -keep-times=true -use-links=true