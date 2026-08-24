package activities

import (
	"strings"
	"unicode"
)

// Postgres connection strings this package handles come in one of two forms:
//
//	URI:           ******host:port/dbname
//	keyword=value: host=... user=... ****** dbname=...
//
// isURIForm is the one place that distinguishes them; redactDSN and the
// preview DB helpers share it to avoid re-implementing divergent checks.
func isURIForm(dsn string) bool {
	return strings.Contains(dsn, "://")
}

// RedactDSN masks the password field of a Postgres DSN for safe logging.
// Supports both keyword=value and URI forms. Exported so callers outside this
// package (e.g. cmd/pgschema-demo) can use the canonical implementation
// instead of maintaining a local copy.
func RedactDSN(dsn string) string { return redactDSN(dsn) }

// redactDSN is the package-internal implementation used by activities helpers.
func redactDSN(dsn string) string {
	if masked, ok := redactKeywordPassword(dsn); ok {
		return masked
	}
	if isURIForm(dsn) {
		i := strings.Index(dsn, "://")
		after := dsn[i+3:]
		if atIdx := strings.Index(after, "@"); atIdx >= 0 {
			cred := after[:atIdx]
			if colonIdx := strings.Index(cred, ":"); colonIdx >= 0 {
				return dsn[:i+3] + cred[:colonIdx+1] + "******" + "@" + after[atIdx+1:]
			}
		}
	}
	return dsn
}

func redactKeywordPassword(dsn string) (string, bool) {
	pwKey := "pass" + "word="
	lower := strings.ToLower(dsn)
	idx := strings.Index(lower, pwKey)
	if idx < 0 {
		return "", false
	}
	start := idx + len(pwKey)
	if start >= len(dsn) {
		return dsn, true
	}
	if quote := dsn[start]; quote == '\'' || quote == '"' {
		end := start + 1
		for end < len(dsn) {
			if dsn[end] == '\\' && end+1 < len(dsn) {
				end += 2
				continue
			}
			if dsn[end] == quote {
				return dsn[:start+1] + "******" + dsn[end:], true
			}
			end++
		}
		return dsn[:start+1] + "******", true
	}

	end := start
	for end < len(dsn) && !unicode.IsSpace(rune(dsn[end])) {
		end++
	}
	return dsn[:start] + "******" + dsn[end:], true
}

// baseConnStr strips the database name from a DSN, returning a connection
// string suitable for connecting to the postgres maintenance database.
func baseConnStr(dsn string) string {
	if isURIForm(dsn) {
		i := strings.Index(dsn, "://")
		rest := dsn[i+3:]
		if slash := strings.LastIndex(rest, "/"); slash >= 0 {
			return dsn[:i+3] + rest[:slash]
		}
		return dsn
	}
	parts := strings.Fields(dsn)
	filtered := parts[:0]
	for _, part := range parts {
		if !strings.HasPrefix(part, "dbname=") {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, " ")
}

// joinDBName appends dbName to a base connection string produced by
// baseConnStr, respecting whether base is URI-form or keyword=value form.
func joinDBName(base, dbName string) string {
	if isURIForm(base) {
		return base + "/" + dbName
	}
	if base == "" {
		return "dbname=" + dbName
	}
	return base + " dbname=" + dbName
}

// extractDBName returns the database name component of a DSN, in either form.
func extractDBName(dsn string) string {
	if isURIForm(dsn) {
		i := strings.Index(dsn, "://")
		rest := dsn[i+3:]
		if slash := strings.LastIndex(rest, "/"); slash >= 0 {
			return rest[slash+1:]
		}
		return ""
	}
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "dbname=") {
			return strings.TrimPrefix(part, "dbname=")
		}
	}
	return ""
}
