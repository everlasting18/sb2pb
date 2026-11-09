package app

// Postgres view definitions (pg_get_viewdef output) -> SQLite SQL for PocketBase view collections.
// This is deliberately small: it rewrites what pg_get_viewdef emits for ordinary views (schema
// prefixes, ::casts, ILIKE) and leaves everything else as is. PocketBase validates the query when
// the view collection is saved, so anything SQLite cannot run is reported as manual work instead
// of being created wrong.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var castTypes = map[string]string{
	"smallint": "INTEGER", "integer": "INTEGER", "int": "INTEGER", "int2": "INTEGER", "int4": "INTEGER", "int8": "INTEGER", "bigint": "INTEGER",
	"boolean": "INTEGER", "bool": "INTEGER",
	"real": "REAL", "float4": "REAL", "float8": "REAL", "double precision": "REAL",
	"numeric": "NUMERIC", "decimal": "NUMERIC",
	"text": "TEXT", "character varying": "TEXT", "varchar": "TEXT", "character": "TEXT", "bpchar": "TEXT", "char": "TEXT",
	"citext": "TEXT", "uuid": "TEXT", "name": "TEXT", "date": "TEXT", "json": "TEXT", "jsonb": "TEXT",
	"timestamp with time zone": "TEXT", "timestamp without time zone": "TEXT", "timestamptz": "TEXT", "timestamp": "TEXT",
}

var (
	reSchemaPrefix = regexp.MustCompile(`(?i)\b(public|extensions)\.`)
	reILike        = regexp.MustCompile(`(?i)\bILIKE\b`)
	reTypeMods     = regexp.MustCompile(`^\s*\(\s*\d+\s*(,\s*\d+\s*)?\)`)
	reTypeWord     = regexp.MustCompile(`^"?([a-z_][a-z0-9_]*)"?`)
)

// pgViewToSQLite rewrites a view body; it returns an error for constructs it knows SQLite lacks.
func pgViewToSQLite(def string) (string, error) {
	sql := strings.TrimSpace(def)
	sql = strings.TrimSuffix(sql, ";")
	sql = reSchemaPrefix.ReplaceAllString(sql, "")
	sql = reILike.ReplaceAllString(sql, "LIKE")
	// pg_get_viewdef prints LIKE/ILIKE as operators. SQLite's LIKE is case-insensitive for ASCII,
	// which matches ILIKE; a case-sensitive LIKE becomes case-insensitive (noted in the report).
	sql = strings.NewReplacer(" !~~* ", " NOT LIKE ", " ~~* ", " LIKE ", " !~~ ", " NOT LIKE ", " ~~ ", " LIKE ").Replace(sql)
	for {
		i := indexOutsideQuotes(sql, "::")
		if i < 0 {
			break
		}
		start := operandStart(sql, i)
		if start < 0 {
			return "", fmt.Errorf("cannot rewrite cast near %q", snippet(sql, i))
		}
		typ, n, isArray := parseCastType(sql[i+2:])
		if typ == "" {
			return "", fmt.Errorf("unsupported cast near %q", snippet(sql, i))
		}
		if isArray {
			return "", fmt.Errorf("array cast ::%s[] has no SQLite equivalent", typ)
		}
		target, ok := castTypes[typ]
		if !ok {
			return "", fmt.Errorf("cast to %s has no SQLite equivalent", typ)
		}
		operand := sql[start:i]
		sql = sql[:start] + "CAST(" + operand + " AS " + target + ")" + sql[i+2+n:]
	}
	return sql, nil
}

var multiWordTypes = []string{"timestamp without time zone", "timestamp with time zone", "time without time zone",
	"time with time zone", "character varying", "double precision"}

// parseCastType reads the type after "::" and returns it with the number of bytes consumed.
func parseCastType(rest string) (typ string, n int, isArray bool) {
	lower := strings.ToLower(rest)
	for _, t := range multiWordTypes {
		if strings.HasPrefix(lower, t) && (len(lower) == len(t) || !isIdent(rune(lower[len(t)]))) {
			typ, n = t, len(t)
			break
		}
	}
	if typ == "" {
		m := reTypeWord.FindStringSubmatch(lower)
		if m == nil {
			return "", 0, false
		}
		typ, n = m[1], len(m[0])
	}
	if m := reTypeMods.FindString(lower[n:]); m != "" {
		n += len(m)
	}
	if strings.HasPrefix(lower[n:], "[]") {
		return typ, n + 2, true
	}
	return typ, n, false
}

// operandStart finds where the expression before a "::" begins: a (...) group, a '...' literal,
// or an identifier/number such as p.id or 42.
func operandStart(s string, end int) int {
	j := end - 1
	for j >= 0 && s[j] == ' ' {
		j--
	}
	if j < 0 {
		return -1
	}
	switch s[j] {
	case ')':
		depth := 0
		for k := j; k >= 0; k-- {
			switch s[k] {
			case ')':
				depth++
			case '(':
				depth--
				if depth == 0 {
					// include a function name directly before the parenthesis: count(x)::int
					f := k
					for f > 0 && (isIdent(rune(s[f-1])) || s[f-1] == '.') {
						f--
					}
					return f
				}
			}
		}
		return -1
	case '\'':
		for k := j - 1; k >= 0; k-- {
			if s[k] == '\'' {
				if k > 0 && s[k-1] == '\'' { // escaped ''
					k--
					continue
				}
				return k
			}
		}
		return -1
	}
	k := j
	for k >= 0 && (isIdent(rune(s[k])) || s[k] == '.' || s[k] == '"') {
		k--
	}
	if k == j {
		return -1
	}
	return k + 1
}

func isIdent(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func indexOutsideQuotes(s, sub string) int {
	inQuote := false
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i] == '\'' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func snippet(s string, i int) string {
	a, b := i-20, i+25
	if a < 0 {
		a = 0
	}
	if b > len(s) {
		b = len(s)
	}
	return strings.Join(strings.Fields(s[a:b]), " ")
}
