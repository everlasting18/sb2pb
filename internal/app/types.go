package app

// Postgres type -> PocketBase field mapping, export expressions and data profiling.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	textTypes  = set("text", "varchar", "bpchar", "citext", "name")
	asTextType = set("xml", "bit", "varbit", "interval", "time", "timetz", "inet", "cidr", "macaddr", "macaddr8", "ltree", "lquery",
		"int4range", "int8range", "numrange", "tsrange", "tstzrange", "daterange", "int4multirange", "int8multirange", "nummultirange",
		"tsmultirange", "tstzmultirange", "datemultirange", "point", "line", "lseg", "box", "path", "polygon", "circle", "tsquery", "oid", "pg_lsn", "uuid")
	timeTypes  = set("timestamptz", "timestamp", "date")
	jsonTypes  = set("json", "jsonb", "hstore")
	reGeoSub   = regexp.MustCompile(`^geo(?:metry|graphy)\((\w+)`)
	reservedPK = []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}
)

func set(v ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range v {
		m[s] = true
	}
	return m
}

func isArray(c *Column) bool { return strings.HasPrefix(c.TypName, "_") }
func isGeo(c *Column) bool   { return c.TypName == "geometry" || c.TypName == "geography" }

// colExpr is the SQL expression used to export a column (inside row_to_json of a subquery).
func colExpr(c *Column) string {
	q := qi(c.Name)
	switch {
	case c.TypName == "money":
		return q + "::numeric" // money renders locale-dependent text ("$1,234.56")
	case c.TypName == "bpchar":
		return q + "::text" // char(n): drop the blank padding, same as Postgres comparison semantics
	case isGeo(c) && c.GeoSub == "point":
		g := q + "::geometry"
		return fmt.Sprintf("case when %s is null or ST_IsEmpty(%s) then null else json_build_object('lon', ST_X(%s), 'lat', ST_Y(%s)) end", q, g, g, g)
	case isGeo(c):
		return fmt.Sprintf("ST_AsGeoJSON(%s::geometry)::json", q)
	case c.TypName == "vector" || c.TypName == "halfvec":
		return q + "::text::json"
	case c.TypName == "bytea":
		return fmt.Sprintf("replace(encode(%s, 'base64'), E'\\n', '')", q)
	case c.TypName == "tsvector":
		return "null::text"
	case c.TypName == "xml":
		return q + "::text" // xml_out (used by to_json) drops the <?xml ...?> declaration
	}
	return q
}

func sourceOf(t *Table) string {
	src := "public." + qi(t.Name)
	if t.Sampled {
		src += fmt.Sprintf(" tablesample system (%g)", t.SamplePct)
	}
	return src
}

func profileColumn(ctx context.Context, db *pgx.Conn, t *Table, c *Column) {
	tbl := sourceOf(t)
	col := qi(c.Name)
	e := colExpr(c)
	if m := reGeoSub.FindStringSubmatch(c.Type); m != nil {
		c.GeoSub = strings.ToLower(m[1])
		e = colExpr(c)
	}
	row := func(sql string, dst ...any) {
		if err := db.QueryRow(ctx, sql).Scan(dst...); err != nil {
			fmt.Printf("  profile %s.%s: %v\n", t.Name, c.Name, err)
		}
	}
	switch {
	case c.TypName == "int2" || c.TypName == "int4" || c.TypName == "int8":
		row(fmt.Sprintf("select coalesce(max(abs(%s::numeric))::float8, 0), coalesce(max(abs(%s::numeric))::text, '0') from %s", col, col, tbl), &c.MaxAbs, &c.MaxAbsText)
	case c.TypName == "numeric" || c.TypName == "money":
		row(fmt.Sprintf(`select coalesce(max(abs(v)) filter (where v not in ('NaN', 'Infinity', '-Infinity'))::float8, 0),
			coalesce(max(abs(v)) filter (where v not in ('NaN', 'Infinity', '-Infinity'))::text, '0'),
			count(*) filter (where v in ('NaN', 'Infinity', '-Infinity')),
			count(*) filter (where v not in ('NaN', 'Infinity', '-Infinity') and (v::float8)::text::numeric <> v)
			from (select %s as v from %s) s where v is not null`, e, tbl), &c.MaxAbs, &c.MaxAbsText, &c.NonFinite, &c.Lossy)
	case c.TypName == "float4" || c.TypName == "float8":
		row(fmt.Sprintf(`select count(*) filter (where %s in ('NaN', 'Infinity', '-Infinity')) from %s`, col, tbl), &c.NonFinite)
	case c.TypName == "bool":
		row(fmt.Sprintf("select count(*) filter (where %s is null) from %s", col, tbl), &c.NullCount)
	case timeTypes[c.TypName]:
		lo, hi := "'0001-01-01'", "'10000-01-01'"
		row(fmt.Sprintf(`select count(*) filter (where not isfinite(%s)), count(*) filter (where isfinite(%s) and (%s < %s or %s >= %s)) from %s`,
			col, col, col, lo, col, hi, tbl), &c.Infinite, &c.OutOfRange)
	case textTypes[c.TypName] || c.TypName == "xml":
		row(fmt.Sprintf("select count(*) filter (where %s::text = ''), coalesce(max(char_length(%s::text)), 0) from %s", col, col, tbl), &c.EmptyCount, &c.MaxLen)
		var nonASCII int64
		row(fmt.Sprintf(`select count(*) filter (where v ~ '[^\x01-\x7F]') from (select %s::text as v from %s where %s is not null limit 1000) s`, col, tbl, col), &nonASCII)
		c.NonASCII = nonASCII > 0
		if c.TypName != "bpchar" && c.TypName != "xml" && hasStorageSchema {
			var nonNull, matched int64
			var bucket string
			row(fmt.Sprintf(`select count(*), count(o.id), coalesce(min(o.bucket_id), '') from
				(select %s::text as v from %s where %s is not null and %s <> '' limit 2000) s
				left join storage.objects o on o.name = regexp_replace(s.v, '^.*/storage/v1/object/(public|authenticated|sign)/[^/]+/', '')`,
				col, tbl, col, col), &nonNull, &matched, &bucket)
			if nonNull > 0 && float64(matched)/float64(nonNull) >= 0.5 {
				c.LinkBucket, c.LinkRatio = bucket, float64(matched)/float64(nonNull)
			}
		}
		if len(t.PK) == 1 && t.PK[0] == c.Name {
			row(fmt.Sprintf(`select count(*) filter (where %s ~ '[[:space:]./\\|"''`+"`"+`<>:?*%%$]' or upper(%s) = any(array['%s']) or char_length(%s) > 100 or %s = '') from %s`,
				col, col, strings.Join(reservedPK, "','"), col, col, tbl), &c.BadPK)
		}
	case c.TypName == "bytea":
		row(fmt.Sprintf("select coalesce(max(octet_length(%s)), 0) from %s", col, tbl), &c.MaxBytes)
	case jsonTypes[c.TypName] || isArray(c) || c.TypType == "c" || isGeo(c) || c.TypName == "vector":
		row(fmt.Sprintf("select coalesce(max(octet_length((%s)::text)), 0) from %s", e, tbl), &c.MaxBytes)
	}
}

// mapColumn decides the PocketBase field for a (non-PK, non-FK) column.
func mapColumn(c *Column, fp *FieldPlan) {
	note := func(s string, a ...any) {
		if fp.Note != "" {
			fp.Note += "; "
		}
		fp.Note += fmt.Sprintf(s, a...)
	}
	switch {
	case c.TypName == "tsvector":
		fp.Kind = "skip"
		note("tsvector dropped (rebuild search separately)")
		return
	case c.TypName == "int2" || c.TypName == "int4":
		fp.Kind, fp.OnlyInt = "number", true
	case c.TypName == "int8":
		if c.MaxAbs > maxSafeInt {
			fp.Kind = "text"
			note("bigint max %s > 2^53 -> text keeps exact digits", c.MaxAbsText)
		} else {
			fp.Kind, fp.OnlyInt = "number", true
		}
	case c.TypName == "numeric" || c.TypName == "money":
		if c.Lossy > 0 {
			fp.Kind = "text"
			note("%d values not exact in float64 -> text keeps exact decimal", c.Lossy)
		} else {
			fp.Kind = "number"
			note("exact in float64 (checked every row)")
		}
		if c.TypName == "money" {
			note("money exported as numeric (not locale text)")
		}
		if c.NonFinite > 0 && fp.Kind == "number" {
			note("%d NaN/Infinity -> 0", c.NonFinite)
		}
	case c.TypName == "float4" || c.TypName == "float8":
		fp.Kind = "number"
		if c.NonFinite > 0 {
			note("%d NaN/Infinity -> 0 (PocketBase numbers must be finite)", c.NonFinite)
		}
	case c.TypName == "bool":
		fp.Kind = "bool"
	case timeTypes[c.TypName]:
		if c.OutOfRange > 0 {
			fp.Kind = "text"
			note("%d values outside year 1..9999 -> text", c.OutOfRange)
			break
		}
		nowDefault := strings.Contains(c.Default, "now()") || strings.Contains(strings.ToUpper(c.Default), "CURRENT_TIMESTAMP")
		if c.Touch != "" {
			fp.Kind, fp.OnUpdate, fp.NoOnCreate = "autodate", true, !nowDefault && !c.TouchOnInsert
			note("trigger %s sets it on every UPDATE -> autodate(onUpdate), original values preserved", c.Touch)
		} else if nowDefault {
			fp.Kind = "autodate"
			note("DEFAULT now() -> autodate(onCreate), original values preserved")
		} else {
			fp.Kind, fp.Required = "date", c.NotNull
		}
		switch c.TypName {
		case "date":
			note("date -> datetime at 00:00Z")
		case "timestamp":
			note("no time zone -> stored as UTC")
		}
		note("µs -> ms")
		if c.Infinite > 0 {
			note("%d ±infinity -> 9999-12-31 / 0001-01-01 sentinels", c.Infinite)
		}
	case textTypes[c.TypName]:
		fp.Kind = "text"
		fp.Required = c.NotNull && c.EmptyCount == 0
		if c.TypName == "bpchar" {
			note("char(n) padding trimmed")
		}
		if c.TypName == "citext" {
			note("citext -> text; unique indexes get COLLATE NOCASE (ASCII-only folding)")
		}
	case asTextType[c.TypName]:
		fp.Kind = "text"
		if c.TypName != "uuid" {
			note("%s -> text", c.TypName)
		}
	case isGeo(c) && c.GeoSub == "point":
		fp.Kind = "geopoint"
		note("PostGIS Point -> geoPoint {lon,lat}")
	case isGeo(c):
		fp.Kind = "json"
		note("PostGIS %s -> GeoJSON", c.GeoSub)
	case c.TypName == "vector" || c.TypName == "halfvec":
		fp.Kind = "json"
		note("pgvector -> json array (no similarity search)")
	case c.TypName == "bytea":
		fp.Kind = "bytes"
		fp.MaxSize = c.MaxBytes
		note("bytea -> file (%d bytes max), protected", c.MaxBytes)
	case jsonTypes[c.TypName]:
		fp.Kind = "json"
		if c.TypName == "hstore" {
			note("hstore -> json object")
		}
		if c.TypName == "json" {
			note("json kept verbatim when PocketBase accepts it; duplicate keys -> jsonb-normalized")
		}
	case c.TypType == "e":
		fp.Kind, fp.Values, fp.MaxSelect, fp.Required = "select", c.EnumValues, 1, c.NotNull
	case isArray(c) && c.ElemType == "e":
		fp.Kind, fp.Values, fp.MaxSelect = "select", c.EnumValues, len(c.EnumValues)
		note("enum[] -> multi select (NULL elements dropped, duplicates collapsed)")
	case isArray(c):
		fp.Kind = "json"
		note("array -> json")
	case c.TypType == "c":
		fp.Kind = "json"
		note("composite type -> json object")
	default:
		fp.Kind = "text"
		note("unknown type %s -> text", c.TypName)
	}
	if c.Domain != "" {
		note("domain %s resolved to %s (CHECK not ported)", c.Domain, c.TypName)
	}
	if c.Generated {
		note("GENERATED column -> plain field (recompute in hook)")
	}
	if c.Identity {
		note("IDENTITY -> no auto-increment in PocketBase")
	}
	if fp.Kind == "text" && c.MaxLen > 5000 {
		fp.Max = int(((c.MaxLen*5/4)/1000 + 1) * 1000)
		note("max length %d > PocketBase default 5000 -> max=%d", c.MaxLen, fp.Max)
	}
	if fp.Kind == "json" && c.MaxBytes > 1<<20*3/4 {
		fp.MaxSize = ((c.MaxBytes*2)/(1<<20) + 1) << 20
		note("largest value %.2f MB, default limit 1 MB -> maxSize=%d MB", float64(c.MaxBytes)/(1<<20), fp.MaxSize>>20)
	}
}
