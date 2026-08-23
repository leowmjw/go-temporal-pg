package activities

import "strings"

// Postgres connection strings this package handles come in one of two forms:
//
//	URI:           postgres://user:pass@host:port/dbname
//	keyword=value: host=... user=... password=... dbname=...
//
// isURIForm is the one place that distinguishes them; redactDSN (moved here
// from pgroll.go) and baseConnStr/joinDBName/extractDBName (moved here from
// preview_db.go) used to each independently re-derive this with their own
// "://" checks.
func isURIForm(dsn string) bool {
	return strings.Contains(dsn, "://")
}

// redactDSN masks the password field for safe logging. Supports
// keyword=value (password=******) and URI (://user:******@host) forms.
func redactDSN(dsn string) string {
	pwKey := " password="
	if i := strings.Index(dsn, pwKey); i >= 0 {
		valStart := i + len(pwKey)
		end := valStart
		if valStart < len(dsn) && dsn[valStart] == '\'' {
			// Quoted value (libpq allows this so the value can contain
			// spaces): skip to the matching, non-escaped closing quote so we
			// don't stop redacting partway through the real password.
			end++
			for end < len(dsn) {
				if dsn[end] == '\\' && end+1 < len(dsn) {
					end += 2
					continue
				}
				if dsn[end] == '\'' {
					end++
					break
				}
				end++
			}
		} else {
			for end < len(dsn) && dsn[end] != ' ' && dsn[end] != '\t' {
				end++
			}
		}
		return dsn[:valStart] + "******" + dsn[end:]
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
	// keyword=value form: strip dbname= token
	parts := strings.Fields(dsn)
	filtered := parts[:0]
	for _, p := range parts {
		if !strings.HasPrefix(p, "dbname=") {
			filtered = append(filtered, p)
		}
	}
	return strings.Join(filtered, " ")
}

// joinDBName appends dbName to a base connection string produced by
// baseConnStr, respecting whether base is URI-form ("scheme://host:port") or
// keyword=value form ("host=... user=..."). Naively concatenating "/dbname"
// onto a keyword=value base corrupts the last keyword's value instead of
// selecting a database.
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
	for _, p := range strings.Fields(dsn) {
		if strings.HasPrefix(p, "dbname=") {
			return strings.TrimPrefix(p, "dbname=")
		}
	}
	return ""
}
