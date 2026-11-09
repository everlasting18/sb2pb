# Cell-by-cell comparison of every Postgres type in public.type_lab (Supabase) vs the migrated PocketBase record,
# plus write-path and semantic checks. Reference values come straight from Postgres (text / json / PostGIS functions).
import json, re, hashlib, urllib.parse, requests, psycopg
from decimal import Decimal

import os, sys
E2E = os.path.dirname(os.path.abspath(__file__))
WORK = os.environ.get("E2E_WORK", os.path.join(E2E, ".work"))
ENV_FILE = os.path.join(E2E, "supabase", "demo.env")
PB = "http://127.0.0.1:8090"
PG = "postgresql://postgres:your-super-secret-and-long-postgres-password@localhost:54322/postgres"
pg = psycopg.connect(PG, autocommit=True)
pg.execute("set time zone 'UTC'; set extra_float_digits = 3; set intervalstyle = 'iso_8601'; set search_path = public, extensions")
SU = {"Authorization": requests.post(f"{PB}/api/collections/_superusers/auth-with-password",
      json={"identity": "owner@pocketbasecloud.test", "password": "Owner-Pass-2026"}).json()["token"]}
dec = lambda txt: json.loads(txt, parse_float=Decimal)
rep = json.load(open(os.path.join(WORK, "sb2pb-report.json")))["migration"]
plan = {f["pg"]: f for p in rep["collections"] if p["pb"] == "type_lab" for f in p["fields"] if f.get("pg")}

ids = [r[0] for r in pg.execute("select id::text from type_lab order by id").fetchall()]
labels = dict(pg.execute("select id::text, label from type_lab").fetchall())
pbtxt = requests.get(f"{PB}/api/collections/type_lab/records?perPage=50&sort=id", headers=SU).text
pbrec = {r["id"]: r for r in dec(pbtxt)["items"]}
ftoken = requests.post(f"{PB}/api/files/token", headers=SU).json()["token"]

def pgcol(expr):
    return dict((i, v) for i, v in pg.execute(f"select id::text, {expr} from type_lab").fetchall())

def norm(v):
    if isinstance(v, bool) or v is None or isinstance(v, str):
        return v
    if isinstance(v, (int, float, Decimal)):
        return Decimal(str(v)).normalize() if v == v else "NaN"
    if isinstance(v, list):
        return [norm(x) for x in v]
    if isinstance(v, dict):
        return {k: norm(x) for k, x in v.items()}
    return v

def ts(t, naive=False):
    m = re.match(r"(\d{4,})-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)(?:\.(\d+))?", t)
    y, mo, d, H, M, S, frac = m.groups()
    frac = (frac or "").ljust(6, "0")
    return f"{y}-{mo}-{d} {H}:{M}:{S}.{frac[:3]}Z", frac[3:] != "000"

cells = {}   # (col, id) -> dict(status, expected, actual, note)
def put(col, i, expected, actual, cls, note=""):
    ok = norm(expected) == norm(actual)
    cells[(col, i)] = {"status": cls if ok else "MISMATCH", "expected": expected, "actual": actual, "note": note}

def simple(col, kind, zero, base_cls="exact", expr=None, convert=None):
    t = pgcol(expr or f"{col}::text")
    for i in ids:
        v, act = t[i], pbrec[i].get(plan[col]["pb"])
        if v is None:
            put(col, i, zero, act, "loss", f"NULL → {json.dumps(zero)}")
        else:
            put(col, i, convert(v) if convert else v, act, base_cls)

num = lambda v: Decimal(v)
for c in ("c_smallint", "c_integer", "c_identity", "c_domain_int"):
    simple(c, "number", 0, "exact", convert=num)
for c in ("c_bigint", "c_bigint_huge"):
    simple(c, "text", "", "converted")                       # > 2^53 -> text, exact digits
simple("c_numeric", "number", 0, "converted", convert=num)   # verified exact in float64
simple("c_numeric_big", "text", "", "converted")
simple("c_money", "text", "", "converted", expr="c_money::numeric::text")
for c in ("c_real", "c_double", "c_double_nan"):
    t = pgcol(f"{c}::text")
    for i in ids:
        v, act = t[i], pbrec[i][c]
        if v is None:
            put(c, i, 0, act, "loss", "NULL → 0")
        elif v in ("NaN", "Infinity", "-Infinity"):
            put(c, i, 0, act, "loss", f"{v} → 0")
        else:
            put(c, i, Decimal(v), act, "exact")
simple("c_bool", "bool", False, "exact", convert=lambda v: v == "true")
for c in ("c_text", "c_varchar", "c_citext", "c_long_text", "c_domain_mail", "c_generated", "c_uuid", "c_xml", "c_enum"):
    simple(c, "text", "", "exact")
simple("c_char", "text", "", "converted")   # bpchar::text drops the padding
for c in ("c_time", "c_timetz", "c_interval", "c_inet", "c_cidr", "c_macaddr", "c_bit", "c_int4range", "c_tstzrange", "c_point", "c_polygon", "c_ltree"):
    simple(c, "text", "", "converted", expr=f"to_json({c})#>>'{{}}'")
t = pgcol("c_date::text")
for i in ids:
    v, act = t[i], pbrec[i]["c_date"]
    if v is None:
        put("c_date", i, "", act, "loss", "NULL → ''")
    elif v == "0001-01-01":
        put("c_date", i, "0001-01-01 00:00:00.001Z", act, "loss", "0001-01-01T00:00Z is PB's empty date → +1ms")
    else:
        put("c_date", i, v + " 00:00:00.000Z", act, "converted")
for c in ("c_timestamp", "c_timestamptz", "c_ts_inf"):
    t = pgcol(f"{c}::text")
    for i in ids:
        v, act = t[i], pbrec[i][c]
        if v is None:
            put(c, i, "", act, "loss", "NULL → ''")
        elif v == "infinity":
            put(c, i, "9999-12-31 23:59:59.999Z", act, "loss", "infinity → sentinel")
        elif v == "-infinity":
            put(c, i, "0001-01-01 00:00:00.001Z", act, "loss", "-infinity → sentinel")
        else:
            exp, cut = ts(v)
            if exp == "0001-01-01 00:00:00.000Z":
                put(c, i, "0001-01-01 00:00:00.001Z", act, "loss", "0001-01-01T00:00Z is PB's empty date → +1ms")
            else:
                put(c, i, exp, act, "loss" if cut else "converted", "µs truncated to ms" if cut else "")
def pb_accepts(txt):
    dup = False
    def hook(pairs):
        nonlocal dup
        if len(set(k for k, _ in pairs)) != len(pairs): dup = True
        return dict(pairs)
    big = False
    def num(x):
        nonlocal big
        if abs(float(x)) == float("inf"): big = True
        return Decimal(x)
    json.loads(txt, object_pairs_hook=hook, parse_float=num, parse_int=num)
    return not dup and not big
t = pgcol("c_json::text"); tb = pgcol("c_json::jsonb::text")
for i in ids:
    v = t[i]
    if v is None:
        put("c_json", i, None, pbrec[i]["c_json"], "exact")
    elif pb_accepts(v):
        put("c_json", i, dec(v), pbrec[i]["c_json"], "exact", "JSON null and SQL NULL both → null" if v == "null" else "verbatim")
    elif pb_accepts(tb[i]):
        put("c_json", i, dec(tb[i]), pbrec[i]["c_json"], "loss", "duplicate keys → jsonb-normalized (last wins)")
    else:
        put("c_json", i, v, pbrec[i]["c_json"], "loss", "number beyond float64 → whole value kept as JSON string")
for c in ("c_jsonb", "c_jsonb_big", "c_int_arr", "c_text_arr", "c_2d_arr", "c_composite"):
    t = pgcol(f"to_json({c})::text")
    for i in ids:
        v = t[i]
        put(c, i, None if v is None else dec(v), pbrec[i][c], "exact" if c.startswith("c_json") else "converted",
            "JSON null and SQL NULL both → null" if v == "null" else "")
t = pgcol("to_json(c_enum_arr)::text")
for i in ids:
    v = None if t[i] is None else dec(t[i])
    if v is None:
        put("c_enum_arr", i, [], pbrec[i]["c_enum_arr"], "loss", "NULL → []")
    else:
        kept = [x for x in v if x is not None]
        put("c_enum_arr", i, kept, pbrec[i]["c_enum_arr"], "loss" if len(kept) != len(v) else "converted", "NULL element dropped" if len(kept) != len(v) else "")
t = pgcol("case when c_geo_point is null then null else json_build_object('lon', ST_X(c_geo_point), 'lat', ST_Y(c_geo_point))::text end")
for i in ids:
    if t[i] is None:
        put("c_geo_point", i, {"lon": 0, "lat": 0}, pbrec[i]["c_geo_point"], "loss", "NULL → (0,0)")
    else:
        put("c_geo_point", i, dec(t[i]), pbrec[i]["c_geo_point"], "converted")
for c, expr in (("c_geo_poly", "ST_AsGeoJSON(c_geo_poly)"), ("c_vector", "c_vector::text"), ("c_hstore", "hstore_to_json(c_hstore)::text")):
    t = pgcol(expr)
    for i in ids:
        put(c, i, None if t[i] is None else dec(t[i]), pbrec[i][c], "converted")
t = pgcol("md5(c_bytea) || ':' || octet_length(c_bytea)")
for i in ids:
    fname = pbrec[i]["c_bytea"]
    if t[i] is None:
        put("c_bytea", i, "", fname, "exact", "NULL → no file")
        continue
    data = requests.get(f"{PB}/api/files/type_lab/{i}/{fname}?token={ftoken}").content
    put("c_bytea", i, t[i], f"{hashlib.md5(data).hexdigest()}:{len(data)}", "converted", "bytea → protected file")
for i in ids:
    put("c_tsvector", i, "(dropped)", "(dropped)" if "c_tsvector" not in pbrec[i] else "present", "loss", "tsvector not migrated")

# ---------- write path + semantics ----------
checks = []
def check(name, expected, actual, ok=None):
    ok = (expected == actual) if ok is None else ok
    checks.append({"group": "Semantics", "name": name, "expected": str(expected), "actual": str(actual), "ok": bool(ok)})
    print(("PASS " if ok else "FAIL ") + f"{name}: expected={expected} actual={actual}")

for i in ids:
    r = requests.patch(f"{PB}/api/collections/type_lab/records/{i}", headers=SU, json={"label": labels[i]})
    check(f"re-save migrated record '{labels[i]}' through API (all field validators run)", 200, r.status_code)

# why the importer sets limits: a collection with PocketBase defaults rejects the same data
requests.delete(f"{PB}/api/collections/limits_demo", headers=SU)
requests.post(f"{PB}/api/collections", headers=SU, json={"name": "limits_demo", "type": "base",
              "fields": [{"name": "t", "type": "text"}, {"name": "j", "type": "json"}, {"name": "n", "type": "number"}]})
r = requests.post(f"{PB}/api/collections/limits_demo/records", headers=SU, json={"t": "Đ" * 120000})
check("default text field (max 5000) rejects the 120k-char value", 400, r.status_code)
big = pg.execute("select c_jsonb_big::text from type_lab where c_jsonb_big is not null").fetchone()[0]
r = requests.post(f"{PB}/api/collections/limits_demo/records", headers={**SU, "Content-Type": "application/json"}, data='{"j": ' + big + '}')
check("default json field (1 MB) rejects the 1.49 MB jsonb value", 400, r.status_code)
r = requests.post(f"{PB}/api/collections/limits_demo/records", headers={**SU, "Content-Type": "application/json"}, data='{"n": "NaN"}')
check("number field cannot hold NaN (why NaN/Infinity become 0)", True, r.status_code == 400 or r.json().get("n") in (0, None), )
requests.delete(f"{PB}/api/collections/limits_demo", headers=SU)

# PK / FK types
city = requests.get(f"{PB}/api/collections/cities/records?expand=country_code,slug&sort=name&perPage=10", headers=SU).json()["items"]
hn = [c for c in city if c["name"] == "Hà Nội"][0]
check("text PK kept as id ('VN') and FK expands", ("VN", "Việt Nam"), (hn["country_code"], hn["expand"]["country_code"]["name"]))
check("text PK with '/' and space -> hashed id, FK remapped, original key kept", "Hà Nội/2026", hn["expand"]["slug"]["slug"])
check("reserved Windows name 'CON' as PK -> hashed id, FK still resolves", "CON", [c for c in city if c["name"] == "Tokyo"][0]["expand"]["slug"]["slug"])
trip = requests.get(f"{PB}/api/collections/trips/records?expand=city_id&perPage=10", headers=SU).json()["items"]
check("identity PK named city_id -> id '1'..'4', FK from trips expands", sorted(["Hà Nội", "Sài Gòn", "NYC", "Tokyo"]), sorted(t["expand"]["city_id"]["name"] for t in trip))

# citext uniqueness
r = requests.post(f"{PB}/api/collections/type_lab/records", headers=SU, json={"label": "dup", "c_domain_mail": "typical@mail.com"})
check("citext UNIQUE: 'typical@mail.com' vs existing 'Typical@Mail.com' rejected (COLLATE NOCASE)", 400, r.status_code)
pgdup = pg.execute("select count(*) from type_lab where c_domain_mail = 'đức@ví.dụ.vn'::extensions.citext").fetchone()[0]
r = requests.post(f"{PB}/api/collections/type_lab/records", headers=SU, json={"label": "dup2", "c_domain_mail": "đức@ví.dụ.vn"})
check("citext non-ASCII: Postgres treats 'đức@ví.dụ.vn' = 'ĐỨC@VÍ.DỤ.vn'; PocketBase NOCASE does not (known gap)", (1, 200), (pgdup, r.status_code))
if r.status_code == 200:
    requests.delete(f"{PB}/api/collections/type_lab/records/{r.json()['id']}", headers=SU)

# case-insensitive search
q = lambda f: requests.get(f"{PB}/api/collections/type_lab/records?filter=" + urllib.parse.quote(f), headers=SU).json()["totalItems"]
pgn = pg.execute("select count(*) from type_lab where c_citext ilike '%hello%'").fetchone()[0]
check("search ASCII case-insensitive: PG ILIKE '%hello%' vs PB `c_citext ~ \"hello\"`", pgn, q('c_citext ~ "hello"'))
pgn = pg.execute("select count(*) from type_lab where c_citext ilike '%đức%'").fetchone()[0]
check("search Vietnamese case-insensitive: PG ILIKE '%đức%' = 1, PB `~ \"đức\"` finds 0 (SQLite LIKE folds ASCII only)", (1, 0), (pgn, q('c_citext ~ "đức"')))

# collation
pgorder = [r[0] for r in pg.execute("select name from names_lab order by name").fetchall()]
pborder = [r["name"] for r in requests.get(f"{PB}/api/collections/names_lab/records?sort=name&perPage=20", headers=SU).json()["items"]]
check("ORDER BY text: Postgres en_US collation vs PocketBase (SQLite BINARY) differ", True, pgorder != pborder)
checks[-1]["expected"], checks[-1]["actual"] = " | ".join(pgorder), " | ".join(pborder)

# bigint stored as text -> string comparison
pgn = pg.execute("select count(*) from type_lab where c_bigint_huge > 100").fetchone()[0]
check("bigint→text: numeric filter becomes string compare (PG `> 100` vs PB `> \"100\"`)", True, q('c_bigint_huge > "100"') != pgn)
pbn = q('c_bigint_huge > "100"')
checks[-1]["expected"], checks[-1]["actual"] = f"PG: {pgn} rows", f"PB: {pbn} rows (lexicographic)"

# infinity sentinel keeps ordering semantics
check("timestamptz 'infinity' sentinel still filters as 'future' (PG `> now()` vs PB `> @now`)", pg.execute("select count(*) from type_lab where c_ts_inf > now()").fetchone()[0], q("c_ts_inf > @now"))
# geo
check("PostGIS point usable in PB geo filter: geoDistance(...) < 5 km from Hoàn Kiếm", 1, q("geoDistance(c_geo_point.lon, c_geo_point.lat, 105.8516, 21.0287) < 5"))

def todisp(v):
    if isinstance(v, Decimal):
        return int(v) if v == v.to_integral_value() else float(v)
    if isinstance(v, dict):
        return {k: todisp(x) for k, x in v.items()}
    if isinstance(v, list):
        return [todisp(x) for x in v]
    return v
json.dump({"cells": {f"{c}|{i}": todisp(v) for (c, i), v in cells.items()}, "ids": ids, "labels": labels, "checks": checks,
           "plan": plan}, open(os.path.join(WORK, "types_results.json"), "w"), default=str, ensure_ascii=False, indent=1)
from collections import Counter
print(Counter(v["status"] for v in cells.values()), f"semantic checks: {sum(c['ok'] for c in checks)}/{len(checks)}")
for (c, i), v in cells.items():
    if v["status"] == "MISMATCH":
        print("MISMATCH", c, labels[i], "expected:", str(v["expected"])[:120], "| actual:", str(v["actual"])[:120])
bad = sum(v["status"] == "MISMATCH" for v in cells.values()) + sum(not c["ok"] for c in checks)
sys.exit(1 if bad else 0)
