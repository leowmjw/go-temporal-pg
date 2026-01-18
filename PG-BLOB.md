# PG BLOB

## Objective

Leverage Postgres as a flexible metadata; keeping easy operations for NEWSQL patterns with Postgres BYTEA + Tigris Object Store. It should be easy + operationally simple upgrade if needed.  Serialization is simple CBOR stream..

## Resources

- LOB Limits - https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/PostgreSQL_large_objects_lo_extension.html
- Load LOB - https://docs.aws.amazon.com/prescriptive-guidance/latest/patterns/load-blob-files-into-text-by-using-file-encoding-in-aurora-postgresql-compatible.html
- PG Blob - https://proopensource.it/blog/postgresql-blobs
- Manaing LOB - https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/PostgreSQL_large_objects_lo_extension.html
- Orphan Blob - https://proopensource.it/blog/postgresql-blobs-part-2
- PG Internals - https://akshitb.medium.com/postgresql-internals-chapter-1-misconceptions-4ce5f0428878
- PG internals Pt2 - https://akshitb.medium.com/postgresql-internals-chapter-2-pages-379196237d9c
- CBOR Stream 

## Example Code - pgx

Use the best native integration driver - pgx

In PostgreSQL, there are two primary ways to store large binary data: BYTEA and Large Objects (LOB) via the OID type. 

=================================================================================
Feature 	 | BYTEA	              | Large Object (OID)
Max Size	 | 1 GB	                      | 4 TB
Storage Location | In-table (via TOAST)	      | Dedicated pg_largeobject table
Access Method	 | Atomic (full read/write)   | Streaming (seek, partial read/write)
Deletion	 | Automatic with row	      | Manual (must use lo_unlink)
================================================================================

### BYTEA 

```go
...

func saveBytea(ctx context.Context, pool *pgxpool.Pool, data []byte) error {
    _, err := pool.Exec(ctx, "INSERT INTO storage (content) VALUES ($1)", data)
    return err
}

```

### LOB

We ignore this as it is extra extension 

```go
func saveLargeObject(ctx context.Context, pool *pgxpool.Pool, r io.Reader) (uint32, error) {
    tx, _ := pool.Begin(ctx)
    defer tx.Rollback(ctx)

    lobs := tx.LargeObjects()
    oid, _ := lobs.Create(ctx, 0)
    
    obj, _ := lobs.Open(ctx, oid, pgx.LargeObjectModeWrite)
    _, err := io.Copy(obj, r) // Stream directly to DB
    obj.Close()
    
    tx.Commit(ctx)
    return uint32(oid), err
}

```

### Tiered Storage

```go
const (
    LimitBytea = 1 * 1024 * 1024 * 1024       // 1 GB
    LimitLOB   = 4 * 1024 * 1024 * 1024 * 1024 // 4 TB
)

func StoreData(ctx context.Context, pool *pgxpool.Pool, r io.Reader, size int64) error {
    switch {
    case size <= LimitBytea:
        // Use BYTEA
        data, _ := io.ReadAll(r)
        return saveBytea(ctx, pool, data)

    case size <= LimitLOB:
        // Use Large Object
        oid, err := saveLargeObject(ctx, pool, r)
        // Store OID in your reference table...
        return err

    default:
        // Use S3 for > 4 TB
        return uploadToS3(ctx, r)
    }
}

```


