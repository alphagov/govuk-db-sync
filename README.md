# GOV.UK Database Sync

`govuk-db-sync` is a robust, multi-threaded pipeline tool designed to safely export, anonymise (transform), compress, upload, download, and restore databases across GOV.UK environments. 

It currently supports **PostgreSQL**, **MySQL**, and **DocumentDB (MongoDB)**.

## Features

- **Safe Atomic Swaps:** Restores data to a temporary database first, applies performance tunings, and performs an atomic rename/swap with the live database (PostgreSQL/MySQL) to ensure zero downtime and prevent corrupted backups from taking down production.
- **Fast & Concurrent:** Utilises parallel threads for dumping and restoring via `mydumper`/`myloader`, `pg_dump`/`pg_restore`, and `mongodump`/`mongorestore`.
- **Data Sanitisation (Transform):** Built-in support for routing data through an isolated "sidecar" database to execute data sanitisation and anonymisation scripts (SQL or JS) before archiving.
- **Highly Optimised Storage:** Uses `tar` with Zstandard (`zst`) compression for incredibly fast and space-efficient backups.
- **S3 Pointer Files:** Automatically maintains `_latest.txt` pointer files in S3 so destination environments always know exactly which archive to pull.
- **Observability:** Native integration with Prometheus Pushgateway to track backup/restore success rates and durations.
- **Smart Defaults:** Automatically extracts database names from connection strings if not explicitly provided.

---

## Prerequisites

To run this pipeline locally, the following external binaries must be available in your `$PATH`:
- **PostgreSQL:** `pg_dump`, `pg_restore`, `psql`
- **MySQL:** `mydumper`, `myloader`, `mysql`, `mysqldump`
- **DocumentDB/MongoDB:** `mongodump`, `mongorestore`, `mongo`
- **Archiving:** `tar`, `zstd`

You must also have valid AWS credentials configured (via `~/.aws/credentials`, environment variables, or IAM roles) with read/write access to your designated S3 bucket.

---

## Commands

### `db-sync backup`
Exports data from the source database, optionally restores it into a local sidecar database to run a transformation/anonymisation script, compresses the output, and uploads it to S3.

### `db-sync restore`
Reads the latest S3 pointer file, downloads the most recent `tar.zst` snapshot, extracts it, and safely restores the data into the destination database.

---

## Configuration & Flags

Configuration can be provided via CLI flags or Environment Variables.

### Global Options
| CLI Flag | Environment Variable | Description |
|---|---|---|
| `--type` | `DB_TYPE` | **Required.** Database engine: `postgres`, `mysql`, or `documentdb`. |
| `--s3-bucket` | `S3_BUCKET` | **Required.** Target AWS S3 bucket name. |
| `--app-name` | `APP_NAME` | Name of the application (used for tagging/logging). |
| `--db-name` | `DB_NAME` | Name of the database. *(Fallback: Automatically extracted from Source/Dest URIs).* |
| `--s3-path` | `S3_PATH` | Custom S3 object prefix/folder to store the backups in. |
| `--s3-region` | `S3_REGION` / `AWS_REGION`| AWS Region for the S3 Bucket (Default: `eu-west-1`). |
| `--threads` | `THREADS` | Number of concurrent threads for dumping/restoring. Set to `0` to auto-detect CPU cores. |
| `--pushgateway-url`| `PUSHGATEWAY_URL` | Prometheus Pushgateway URL for reporting metrics. |
| `--dry-run` | `DRY_RUN` | Simulates the pipeline without executing shell commands or DB operations. |

### Backup Specific Options
| CLI Flag | Environment Variable | Description |
|---|---|---|
| `--source` | `SOURCE_URI` | **Required.** Full connection string to the source database. |
| `--source-password`| `SOURCE_PASSWORD` | Overrides the password in the `--source` URI. |
| `--transform-uri` | `TRANSFORM_URI` | Connection string to the local sidecar database used for sanitisation. |
| `--transform-password`|`TRANSFORM_PASSWORD`| Overrides the password in the `--transform-uri`. |
| `--transform-script`| `TRANSFORM_SCRIPT` | Path to the `.sql` or `.js` file containing data sanitisation instructions. |

### Restore Specific Options
| CLI Flag | Environment Variable | Description |
|---|---|---|
| `--dest` | `DEST_URI` | **Required.** Full connection string to the destination database. |
| `--dest-password` | `DEST_PASSWORD` | Overrides the password in the `--dest` URI. |
| `--db-owner` | `DB_OWNER` | (Postgres only) The role that should own the restored database and public schema. |
| `--docdb-source-db`| `DOCDB_SOURCE_DB` | (DocumentDB only) The original production database name, required to map the namespace safely into the destination. |

---

## Local Testing & Examples

Below are examples of how to test the script locally. Note that when using `--transform-script`, you should ensure you have a "sidecar" database running locally (e.g., via Docker Compose) matching the `--transform-uri`.

### Running via Docker Compose

To run the application within Docker Compose, which isolates the database clients, you can use something like:

```bash
docker compose build
docker compose run --rm db-sync backup --help
```

### 1. PostgreSQL Pipeline

**Backup & Transform:**
```bash
govuk-db-sync backup \
  --type postgres \
  --source "postgres://admin:secret@live-db:5432/my_app" \
  --transform-uri "postgres://localuser:localpass@localhost:5432/my_app_sidecar" \
  --transform-script "./transform_scripts/my_app.sql" \
  --s3-bucket "my-local-bucket"
```

**Restore:**
```bash
govuk-db-sync restore \
  --type postgres \
  --dest "postgres://localuser:localpass@localhost:5432/my_app_integration" \
  --db-owner "app_user" \
  --s3-bucket "my-local-bucket"
```

### 2. MySQL Pipeline

**Backup & Transform:**
```bash
govuk-db-sync backup \
  --type mysql \
  --source "mysql://admin:secret@live-db:3306/my_app" \
  --transform-uri "mysql://localuser:localpass@localhost:3306/my_app_sidecar" \
  --transform-script "./transform_scripts/my_app.sql" \
  --s3-bucket "my-local-bucket"
```

**Restore:**
```bash
govuk-db-sync restore \
  --type mysql \
  --dest "mysql://localuser:localpass@localhost:3306/my_app_integration" \
  --s3-bucket "my-local-bucket"
```

### 3. DocumentDB (MongoDB) Pipeline

*Note: For DocumentDB, transform scripts must be valid JavaScript `.js` files executable by the `mongo` shell.*

**Backup & Transform:**
```bash
govuk-db-sync backup \
  --type documentdb \
  --source "mongodb://admin:secret@live-db:27017/my_app?authSource=admin" \
  --transform-uri "mongodb://localuser:localpass@localhost:27017/my_app_sidecar?authSource=admin" \
  --transform-script "./transform_scripts/my_app_redaction.js" \
  --s3-bucket "my-local-bucket"
```

**Restore:**
*Because MongoDB/DocumentDB collections are rigidly tied to their namespace, restoring to a different database name requires passing the original source database name so `govuk-db-sync` can map `--nsFrom` to `--nsTo`.*
```bash
govuk-db-sync restore \
  --type documentdb \
  --dest "mongodb://localuser:localpass@localhost:27017/my_app_integration?authSource=admin" \
  --docdb-source-db "my_app" \
  --s3-bucket "my-local-bucket"
```

---

## Writing Transform Scripts

Transform scripts run against the intermediate "sidecar" database after the raw production data has been imported, but before it is dumped and uploaded to S3.

**For PostgreSQL / MySQL (`.sql`):**
```sql
-- Remove user PII but preserve referential integrity
UPDATE users SET email = CONCAT('user_', id, '@anonymised.gov.uk'), name = 'REDACTED';
DELETE FROM sensitive_audit_logs WHERE created_at < NOW() - INTERVAL '30 days';
```

**For DocumentDB / MongoDB (`.js`):**
```javascript
// Remove PII from documents
db.users.find().forEach(function(doc) {
    doc.name = 'REDACTED';
    doc.email = 'user_' + doc._id + '@anonymised.gov.uk';
    db.users.save(doc);
});
```

---

## Metrics & Monitoring
If `--pushgateway-url` is provided, `govuk-db-sync` will push the following Prometheus metrics upon completion:
- `govuk_db_sync_status` (1 for success, 0 for failure)
- `govuk_db_sync_duration_seconds`
- `govuk_db_sync_last_completed_timestamp_seconds`

Metrics are automatically grouped by `database_engine`, `database_instance`, `database_db_name`, and `operation` (`backup` or `restore`).
