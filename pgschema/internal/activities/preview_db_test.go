package activities

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// ─────────────────────────────────────────────────────────────────────────────
// Why these test scenarios?
//
//  1. CloneDatabase_Success — validates the clone returns a well-formed target
//     DSN for downstream activities.  A broken DSN here fails the whole preview
//     pipeline silently; wrapping ensures it's caught early.
//
//  2. CloneDatabase_Failure_WrapsPreviewError — clone errors are wrapped in
//     PreviewError so the workflow can route them to human alerting via
//     errors.AsType[*types.PreviewError] (Go 1.26+).
//
//  3. ApplyAnonymization_Success / _Failure — PII removal is the most critical
//     safety step.  A failure MUST block ExposePreviewEndpoint — the workflow
//     enforces this with activity sequencing but the activity itself must also
//     return a clean error for the workflow to handle.
//
//  4. RunMigrationPreview_Success / _Failure — the "dry run" migration.  On
//     failure we want to know the clone can't absorb the migration, which is
//     actionable: fix the migration before going to production.
//
//  5. ExposePreviewEndpoint_TTL — confirms ExpiresAt is set correctly.  Wrong
//     expiry means either premature cleanup (broken dev experience) or leaked
//     PII-adjacent data living too long.
//
//  6. DropPreviewDatabase_Success / _Failure — cleanup is the final safety net.
//     A drop failure must not be silently swallowed — it needs to page a human
//     so the preview DB doesn't persist beyond its TTL.
//
//  7. previewDBName_Deterministic (synctest) — the name generator must be
//     idempotent: re-running the workflow with the same PreviewID must not
//     create a second DB.  Tested in a synctest bubble to confirm no goroutine
//     or global state bleeds between calls.
//
//  8. DSN helpers — baseConnStr and extractDBName correctness on both URI and
//     keyword=value forms.
// ─────────────────────────────────────────────────────────────────────────────

func newTestPreviewDBActivities(
	CloneFn func(context.Context, types.PreviewCloneInput) (string, error),
	anonFn func(context.Context, types.AnonymizationInput) error,
	migPreviewFn func(context.Context, string, string) error,
	DropFn func(context.Context, string) error,
) *PreviewDBActivities {
	a := &PreviewDBActivities{log: newTestLogger()}
	if CloneFn != nil {
		a.CloneFn = CloneFn
	} else {
		a.CloneFn = func(_ context.Context, in types.PreviewCloneInput) (string, error) {
			return "host=localhost dbname=preview_" + in.PreviewID, nil
		}
	}
	if anonFn != nil {
		a.ApplyAnonymizationFn = anonFn
	} else {
		a.ApplyAnonymizationFn = func(_ context.Context, _ types.AnonymizationInput) error { return nil }
	}
	if migPreviewFn != nil {
		a.RunMigrationPreviewFn = migPreviewFn
	} else {
		a.RunMigrationPreviewFn = func(_ context.Context, _, _ string) error { return nil }
	}
	if DropFn != nil {
		a.DropFn = DropFn
	} else {
		a.DropFn = func(_ context.Context, _ string) error { return nil }
	}
	return a
}

// ── CloneDatabase ─────────────────────────────────────────────────────────────

func TestCloneDatabase_Success(t *testing.T) {
	a := newTestPreviewDBActivities(
		func(_ context.Context, in types.PreviewCloneInput) (string, error) {
			return "host=localhost dbname=preview_abc123", nil
		},
		nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.CloneDatabase)
	val, err := env.ExecuteActivity(a.CloneDatabase, types.PreviewCloneInput{
		SourceDSN: "host=localhost dbname=production",
		PreviewID: "abc123",
	})
	require.NoError(t, err)
	var dsn string
	require.NoError(t, val.Get(&dsn))
	assert.Contains(t, dsn, "preview_abc123")
}

func TestCloneDatabase_Failure_WrapsPreviewError(t *testing.T) {
	a := newTestPreviewDBActivities(
		func(_ context.Context, in types.PreviewCloneInput) (string, error) {
			return "", errors.New("pg_dump: connection refused")
		},
		nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.CloneDatabase)
	_, err := env.ExecuteActivity(a.CloneDatabase, types.PreviewCloneInput{
		SourceDSN: "host=localhost dbname=production",
		PreviewID: "xyz",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xyz", "error must contain the preview ID")
}

// ── ApplyAnonymization ────────────────────────────────────────────────────────

func TestApplyAnonymization_Success(t *testing.T) {
	called := false
	a := newTestPreviewDBActivities(nil,
		func(_ context.Context, in types.AnonymizationInput) error {
			called = true
			assert.Equal(t, 2, len(in.Rules))
			return nil
		},
		nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.ApplyAnonymization)
	_, err := env.ExecuteActivity(a.ApplyAnonymization, types.AnonymizationInput{
		TargetDSN: "host=localhost dbname=preview",
		Rules: []types.AnonymizationRule{
			{Table: "users", Column: "email", Transformer: "email"},
			{Table: "users", Column: "name", Transformer: "name"},
		},
	})
	require.NoError(t, err)
	assert.True(t, called)
}

func TestApplyAnonymization_Failure(t *testing.T) {
	a := newTestPreviewDBActivities(nil,
		func(_ context.Context, _ types.AnonymizationInput) error {
			return errors.New("transformer 'unknown' not found")
		},
		nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.ApplyAnonymization)
	_, err := env.ExecuteActivity(a.ApplyAnonymization, types.AnonymizationInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "anonymization failed")
}

// ── RunMigrationPreview ───────────────────────────────────────────────────────

func TestRunMigrationPreview_Success(t *testing.T) {
	a := newTestPreviewDBActivities(nil, nil,
		func(_ context.Context, dsn, mig string) error {
			assert.Equal(t, "host=localhost dbname=preview", dsn)
			assert.Contains(t, mig, "operations")
			return nil
		},
		nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.RunMigrationPreview)
	_, err := env.ExecuteActivity(a.RunMigrationPreview,
		"host=localhost dbname=preview",
		`{"name":"add_col","operations":[]}`)
	require.NoError(t, err)
}

func TestRunMigrationPreview_Failure(t *testing.T) {
	a := newTestPreviewDBActivities(nil, nil,
		func(_ context.Context, _, _ string) error {
			return errors.New("column 'id' of relation 'orders' does not exist")
		},
		nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.RunMigrationPreview)
	_, err := env.ExecuteActivity(a.RunMigrationPreview, "dsn", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "migration preview failed")
}

// ── ExposePreviewEndpoint ─────────────────────────────────────────────────────

func TestExposePreviewEndpoint_TTL(t *testing.T) {
	a := newTestPreviewDBActivities(nil, nil, nil, nil)
	ttl := 2 * time.Hour

	before := time.Now()
	ep, err := a.ExposePreviewEndpoint(context.Background(), "host=localhost dbname=preview", ttl)
	after := time.Now()

	require.NoError(t, err)
	assert.Equal(t, "host=localhost dbname=preview", ep.DSN)
	assert.True(t, ep.ExpiresAt.After(before.Add(ttl-time.Second)),
		"ExpiresAt should be approximately now+TTL")
	assert.True(t, ep.ExpiresAt.Before(after.Add(ttl+time.Second)),
		"ExpiresAt should be approximately now+TTL")
}

// ── DropPreviewDatabase ───────────────────────────────────────────────────────

func TestDropPreviewDatabase_Success(t *testing.T) {
	dropped := ""
	a := newTestPreviewDBActivities(nil, nil, nil,
		func(_ context.Context, dsn string) error {
			dropped = dsn
			return nil
		},
	)

	env := newActEnv(t)
	env.RegisterActivity(a.DropPreviewDatabase)
	_, err := env.ExecuteActivity(a.DropPreviewDatabase, "host=localhost dbname=preview_abc")
	require.NoError(t, err)
	assert.Equal(t, "host=localhost dbname=preview_abc", dropped)
}

func TestDropPreviewDatabase_Failure(t *testing.T) {
	a := newTestPreviewDBActivities(nil, nil, nil,
		func(_ context.Context, _ string) error {
			return errors.New("database does not exist")
		},
	)

	env := newActEnv(t)
	env.RegisterActivity(a.DropPreviewDatabase)
	_, err := env.ExecuteActivity(a.DropPreviewDatabase, "host=localhost dbname=gone")
	require.Error(t, err)
}

// ── previewDBName idempotency ─────────────────────────────────────────────────
// Use synctest to guarantee that two calls with the same input in the same
// goroutine universe produce the same name — detecting any accidental global
// state or time-seeded randomness.

func TestPreviewDBName_Deterministic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		name1 := previewDBName("preview-2026-workflow-1")
		name2 := previewDBName("preview-2026-workflow-1")
		assert.Equal(t, name1, name2, "same PreviewID must always yield the same DB name")

		// Different IDs must yield different names.
		other := previewDBName("preview-2026-workflow-2")
		assert.NotEqual(t, name1, other)

		// Must start with valid prefix for Postgres identifiers.
		assert.True(t, len(name1) > 0)
		assert.Equal(t, "preview_", name1[:8])
	})
}

// ── DSN helpers ───────────────────────────────────────────────────────────────

func TestBaseConnStr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "host=localhost port=5432 dbname=mydb user=pg",
			want: "host=localhost port=5432 user=pg",
		},
		{
			in:   "host=localhost dbname=mydb",
			want: "host=localhost",
		},
	}
	for _, tc := range cases {
		got := baseConnStr(tc.in)
		assert.Equal(t, tc.want, got)
	}
}

func TestExtractDBName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"host=localhost dbname=mydb user=pg", "mydb"},
		{"host=localhost", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, extractDBName(tc.in))
	}
}

// ── joinDBName ────────────────────────────────────────────────────────────────
//
// Regression test: defaultClone used to build the target DSN as
// baseConnStr(dsn)+"/"+dbName unconditionally, which only produces a valid
// connection string for URI-form DSNs. For keyword=value DSNs it corrupted
// the last keyword's value instead of selecting a database (e.g.
// "...sslmode=disable/preview_x" instead of "...sslmode=disable
// dbname=preview_x"), and the corrupted string could not be round-tripped
// back through extractDBName in defaultDrop, silently leaking the preview DB.

func TestJoinDBName(t *testing.T) {
	cases := []struct {
		name string
		base string
		db   string
		want string
	}{
		{
			name: "URI form appends /dbname",
			base: "scheme://localhost:5432",
			db:   "preview_abc",
			want: "scheme://localhost:5432/preview_abc",
		},
		{
			name: "keyword=value form appends dbname= token",
			base: "host=localhost port=5432 user=pg",
			db:   "preview_abc",
			want: "host=localhost port=5432 user=pg dbname=preview_abc",
		},
		{
			name: "empty base still produces a usable dbname= token",
			base: "",
			db:   "preview_abc",
			want: "dbname=preview_abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, joinDBName(tc.base, tc.db))
		})
	}
}

// TestJoinDBName_RoundTripsThroughExtractDBName is the direct regression
// test for the DSN-corruption bug: for both URI and keyword=value source
// DSNs, whatever defaultClone builds via baseConnStr+joinDBName must be
// recoverable by extractDBName the way defaultDrop needs it to be, or the
// preview database silently leaks on cleanup.
func TestJoinDBName_RoundTripsThroughExtractDBName(t *testing.T) {
	cases := []string{
		"scheme://localhost:5432/production",
		"host=localhost port=5432 user=pg dbname=production",
	}
	for _, sourceDSN := range cases {
		base := baseConnStr(sourceDSN)
		target := joinDBName(base, "preview_xyz")
		assert.Equal(t, "preview_xyz", extractDBName(target),
			"dbname must round-trip for source DSN %q (base=%q, target=%q)", sourceDSN, base, target)
	}
}

// ── marshalAnonymizationConfig ──────────────────────────────────────────────
//
// Regression test: defaultApplyAnonymization used to ignore input.Rules
// entirely and run pgstream snapshot mode as a bare passthrough, even though
// callers treat it as a "MUST succeed before expose" PII-scrubbing gate.

func TestMarshalAnonymizationConfig(t *testing.T) {
	rules := []types.AnonymizationRule{
		{Table: "users", Column: "email", Transformer: "email"},
		{Table: "users", Column: "name", Transformer: "name"},
	}
	data, err := marshalAnonymizationConfig(rules)
	require.NoError(t, err)

	var decoded struct {
		Transformers []types.AnonymizationRule `json:"transformers"`
	}
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Transformers, 2)
	assert.Equal(t, "users", decoded.Transformers[0].Table)
	assert.Equal(t, "email", decoded.Transformers[0].Column)
	assert.Equal(t, "email", decoded.Transformers[0].Transformer)
}
