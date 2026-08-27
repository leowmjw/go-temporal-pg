package main

import (
	"context"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgroll keeps each migration version as a real, queryable Postgres schema
// named "<schema>_<version>" (progress.LatestSchema is already one of
// these). Both the old and new versioned schemas coexist during expand;
// `pgroll complete` drops the old one, `pgroll rollback` drops the new one.
// The functions below read that live state directly (short-lived
// connections, same pattern as resetDatabase) rather than adding new
// Temporal activities/queries for it.

type versionColumn struct {
	Name     string
	DataType string
	Nullable bool
}

// listVersionedSchemas returns every "<schema>_%" schema currently in
// Postgres, oldest first (pgroll names them so lexical order == creation
// order).
func listVersionedSchemas(ctx context.Context, dsn, schema string) ([]string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx,
		`SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1 ESCAPE '\' ORDER BY schema_name`,
		strings.ReplaceAll(schema, "_", `\_`)+`\_%`)
	if err != nil {
		return nil, fmt.Errorf("list versioned schemas: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func columnsForSchema(ctx context.Context, dsn, versionedSchema string) ([]versionColumn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx,
		`SELECT column_name, data_type, is_nullable = 'YES' FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'users' ORDER BY ordinal_position`, versionedSchema)
	if err != nil {
		return nil, fmt.Errorf("list columns for %s: %w", versionedSchema, err)
	}
	defer rows.Close()

	var out []versionColumn
	for rows.Next() {
		var c versionColumn
		if err := rows.Scan(&c.Name, &c.DataType, &c.Nullable); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// backfillCoverage reports how many of the total rows in versionedSchema's
// users table have a non-null value in column — the concrete before/after
// signal for scenarios that backfill existing rows.
func backfillCoverage(ctx context.Context, dsn, versionedSchema, column string) (populated, total int, err error) {
	conn, connErr := pgx.Connect(ctx, dsn)
	if connErr != nil {
		return 0, 0, fmt.Errorf("connect: %w", connErr)
	}
	defer conn.Close(ctx)

	q := fmt.Sprintf(`SELECT count(*) FILTER (WHERE %s IS NOT NULL), count(*) FROM %s.users`,
		pgx.Identifier{column}.Sanitize(), pgx.Identifier{versionedSchema}.Sanitize())
	if err := conn.QueryRow(ctx, q).Scan(&populated, &total); err != nil {
		return 0, 0, fmt.Errorf("backfill coverage for %s.%s: %w", versionedSchema, column, err)
	}
	return populated, total, nil
}

// renderVersionPanel builds the side-by-side version cards. cur.SeenVersions
// is the union of every versioned schema ever observed live for this run;
// any of those no longer present in `live` has been physically dropped by
// pgroll (complete/rollback) and renders greyed out and unselectable.
func renderVersionPanel(cur *run, live []string, cols map[string][]versionColumn, backfill *backfillInfo) template.HTML {
	liveSet := map[string]bool{}
	for _, s := range live {
		liveSet[s] = true
	}
	seen := make([]string, 0, len(cur.SeenVersions))
	for s := range cur.SeenVersions {
		seen = append(seen, s)
	}
	sort.Strings(seen)

	if len(seen) == 0 {
		return template.HTML(`<div id="version-panel" class="version-panel"><p class="muted">no versioned schemas yet — waiting for expand to start</p></div>`)
	}

	var b strings.Builder
	b.WriteString(`<div id="version-panel" class="version-panel"><h3>Live schema versions</h3><div class="version-row">`)
	for i, s := range seen {
		cleaned := !liveSet[s]
		active := s == cur.ActiveVersionSchema
		label := "baseline"
		if i == len(seen)-1 {
			label = "latest"
		}
		classes := "version-card"
		if active {
			classes += " active"
		}
		if cleaned {
			classes += " cleaned"
		}

		fmt.Fprintf(&b, `<div class="%s">`, classes)
		fmt.Fprintf(&b, `<div class="version-card-head"><span class="badge idle">%s</span><code>%s</code></div>`,
			label, template.HTMLEscapeString(s))
		b.WriteString(`<ul class="version-cols">`)
		for _, c := range cols[s] {
			null := "NOT NULL"
			if c.Nullable {
				null = "NULL"
			}
			fmt.Fprintf(&b, `<li><code>%s</code> <span class="muted">%s %s</span></li>`,
				template.HTMLEscapeString(c.Name), template.HTMLEscapeString(c.DataType), null)
		}
		b.WriteString(`</ul>`)
		if cleaned {
			b.WriteString(`<p class="muted">dropped — migration finalized this version away</p>`)
		} else {
			fmt.Fprintf(&b, `<button %s data-on:click="@post('/versions/%s/activate')">Preview this version</button>`,
				disabledIf(active), template.URLQueryEscaper(s))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)

	if backfill != nil {
		fmt.Fprintf(&b, `<p class="backfill-status">Backfill (<code>%s</code>): <strong>%d / %d</strong> rows populated%s</p>`,
			template.HTMLEscapeString(backfill.Column), backfill.Populated, backfill.Total,
			backfillHint(backfill))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// renderVersionPanelString adapts renderVersionPanel's template.HTML result
// (built from a *versionSnapshot gathered once per tick) to the plain string
// patchElements expects.
func renderVersionPanelString(cur *run, snap *versionSnapshot) string {
	return string(renderVersionPanel(cur, snap.live, snap.cols, snap.backfill))
}

type backfillInfo struct {
	Column    string
	Populated int
	Total     int
}

func backfillHint(b *backfillInfo) string {
	if b.Total > 0 && b.Populated == b.Total {
		return ` <span class="badge completed">done</span>`
	}
	return ` <span class="badge running">in progress</span>`
}

func disabledIf(cond bool) string {
	if cond {
		return "disabled"
	}
	return ""
}

// versionSnapshot bundles everything streamProgress needs to refresh the
// version panel in one tick, so callers only make the Postgres round trips
// once per tick regardless of how many cards are rendered.
type versionSnapshot struct {
	live     []string
	cols     map[string][]versionColumn
	backfill *backfillInfo
}

func gatherVersionSnapshot(ctx context.Context, dsn, schema string, cur *run) (*versionSnapshot, error) {
	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	live, err := listVersionedSchemas(qCtx, dsn, schema)
	if err != nil {
		return nil, err
	}

	for _, s := range live {
		cur.SeenVersions[s] = true
	}
	if cur.ActiveVersionSchema == "" && len(live) > 0 {
		cur.ActiveVersionSchema = live[len(live)-1]
	}

	cols := make(map[string][]versionColumn, len(cur.SeenVersions))
	for s := range cur.SeenVersions {
		c, err := columnsForSchema(qCtx, dsn, s)
		if err != nil {
			continue // best-effort — a schema dropped mid-query just renders with no columns
		}
		cols[s] = c
	}

	var bf *backfillInfo
	if cur.Scenario.BackfillColumn != "" && len(live) > 0 {
		newest := live[len(live)-1]
		populated, total, err := backfillCoverage(qCtx, dsn, newest, cur.Scenario.BackfillColumn)
		if err == nil {
			bf = &backfillInfo{Column: cur.Scenario.BackfillColumn, Populated: populated, Total: total}
		}
	}

	return &versionSnapshot{live: live, cols: cols, backfill: bf}, nil
}
