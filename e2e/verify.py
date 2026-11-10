# End-to-end verification: Supabase (source, real self-hosted stack) vs PocketBase v0.41.1 (target, after restore)
import hashlib, json, sqlite3, urllib.parse, requests, psycopg

import os, sys
E2E = os.path.dirname(os.path.abspath(__file__))
WORK = os.environ.get("E2E_WORK", os.path.join(E2E, ".work"))
ENV_FILE = os.path.join(E2E, "supabase", "demo.env")
PB = "http://127.0.0.1:8090"; AUTH = "http://localhost:9999"; STORAGE = "http://localhost:5000"; IDP = "http://172.17.0.1:9555"
PG = "postgresql://postgres:your-super-secret-and-long-postgres-password@localhost:54322/postgres"
SERVICE = open(ENV_FILE).read().split("SERVICE_ROLE_KEY=")[1].split("\n")[0]
seed = json.load(open(os.path.join(WORK, "seed_out.json")))
pg = psycopg.connect(PG, autocommit=True); pg.execute("set time zone 'UTC'")
q = lambda sql, *a: pg.execute(sql, a).fetchone()[0]
lite = sqlite3.connect(os.path.join(WORK, "pb", "pb_data", "data.db"))
results = []

def check(group, name, expected, actual, ok=None):
    ok = (expected == actual) if ok is None else ok
    results.append({"group": group, "name": name, "expected": str(expected), "actual": str(actual), "ok": bool(ok)})
    print(("PASS " if ok else "FAIL ") + f"[{group}] {name}: expected={expected} actual={actual}")

def pb_login(email, pw):
    return requests.post(f"{PB}/api/collections/users/auth-with-password", json={"identity": email, "password": pw})

su = requests.post(f"{PB}/api/collections/_superusers/auth-with-password", json={"identity": "owner@pocketbasecloud.test", "password": "Owner-Pass-2026"})
SU = {"Authorization": su.json().get("token", "")}

# ---------- A. delivery: baseline settings survive the restore ----------
check("Delivery", "Superuser (created before migration) still logs in", 200, su.status_code)
check("Delivery", "App name setting preserved", "Customer App · Pocketbasecloud", requests.get(f"{PB}/api/settings", headers=SU).json()["meta"]["appName"])
users_coll = requests.get(f"{PB}/api/collections/users", headers=SU).json()
check("Delivery", "OAuth2 provider config preserved", ["oidc"], [p["name"] for p in users_coll["oauth2"]["providers"]])
bk = [b["key"] for b in requests.get(f"{PB}/api/backups", headers=SU).json()]
check("Delivery", "Rollback point exists (pre_migration backup)", True, any(k.startswith("pre_migration") for k in bk))

# ---------- B. data fidelity ----------
def pb_count(c): return requests.get(f"{PB}/api/collections/{c}/records?perPage=1&skipTotal=0", headers=SU).json()["totalItems"]
check("Data", "users count (non-anonymous)", q("select count(*) from auth.users where not is_anonymous"), pb_count("users"))
for t in ("posts", "comments", "post_likes"):
    check("Data", f"{t} row count", q(f"select count(*) from public.{t}"), pb_count(t))
src = q("select md5(string_agg(id::text || '|' || title || '|' || status::text || '|' || views::text, ',' order by id::text)) from public.posts")
dst = hashlib.md5(",".join(f"{i}|{t}|{s}|{int(v)}" for i, t, s, v in lite.execute("select id, title, status, views from posts order by id")).encode()).hexdigest()
check("Data", "posts checksum md5(id|title|status|views)", src, dst)
src = q("select md5(string_agg(id::text || '|' || post_id::text || '|' || user_id::text || '|' || content, ',' order by id::text)) from public.comments")
dst = hashlib.md5(",".join(f"{i}|{p}|{u}|{c}" for i, p, u, c in lite.execute("select id, post_id, user_id, content from comments order by id")).encode()).hexdigest()
check("Data", "comments checksum md5(id|post|user|content)", src, dst)

e = requests.get(f"{PB}/api/collections/posts/records/11111111-2222-4333-8444-555555555555", headers=SU).json()
row = pg.execute("select big_counter::text, rating::float8, is_featured, published_at, tags, metadata, body, created_at from public.posts where id='11111111-2222-4333-8444-555555555555'").fetchone()
check("Data", "UUID primary key kept as record id", "11111111-2222-4333-8444-555555555555", e["id"])
check("Data", "bigint > 2^53 kept exact (as text)", row[0], e["big_counter"])
check("Data", "numeric(3,1) 4.5", row[1], e["rating"])
check("Data", "NULL boolean -> false (documented)", "None -> False", f"{row[2]} -> {e['is_featured']}")
check("Data", "timestamptz -> UTC, ms precision", "2024-02-02 21:05:06.123Z", e["published_at"])
check("Data", "text[] -> json array", row[4], e["tags"])
check("Data", "jsonb nested", row[5], e["metadata"])
check("Data", "unicode/emoji text", row[6], e["body"])
check("Data", "created_at (DEFAULT now() -> autodate) value preserved", "2024-02-01 10:00:00.000Z", e["created_at"])
a = requests.get(f"{PB}/api/collections/users/records/{seed['alice']['id']}", headers=SU).json()
check("Data", "profiles merged into users (username, website, birthday)", ("alice", "https://alice.dev", "1995-04-12 00:00:00.000Z"), (a["username"], a["website"], a["birthday"]))
check("Data", "profiles.phone kept next to the sign-in phone (users.profile_phone, users.phone untouched)", ("0900 000 001", ""), (a.get("profile_phone"), a["phone"]))
us = requests.get(f"{PB}/api/collections/user_settings/records/{seed['alice']['id']}", headers=SU).json()
check("Data", "second per-user table (user_settings, PK user_id) is its own collection, record id = user id", ("dark", "vi"), (us.get("theme"), us.get("locale")))
check("Data", "user_settings row count", q("select count(*) from public.user_settings"), pb_count("user_settings"))
dangling = json.load(open(os.path.join(WORK, "sb2pb-report.json")))["migration"]["files"]["danglingRelations"]
check("Data", "dangling relations after load", 0, sum(dangling.values()))

# ---------- B2. view ----------
edge = "11111111-2222-4333-8444-555555555555"
v = requests.get(f"{PB}/api/collections/post_stats/records/{edge}", headers=SU).json()
check("View", "post_stats view (Postgres -> SQLite) comment_count matches", q("select comment_count from public.post_stats where id=%s", edge), v.get("comment_count"))

# ---------- C. auth ----------
for k in ("alice", "bob", "carol", "dave"):
    r = pb_login(seed[k]["email"], seed[k]["password"])
    rid = r.json().get("record", {}).get("id")
    check("Auth", f"{k} logs in with ORIGINAL Supabase password" + (" (6 chars)" if k == "carol" else ""), (200, seed[k]["id"]), (r.status_code, rid))
check("Auth", "wrong password rejected", 400, pb_login("alice@example.com", "nope-nope").status_code)
r = pb_login("erin@example.com", "erin-password")
check("Auth", "banned user (banned_until) blocked by authRule", True, r.status_code in (400, 403), )
erin = seed["erin"]["id"]; ban = requests.get(f"{PB}/api/collections/users/records/{erin}", headers=SU).json()["banned_until"]
requests.patch(f"{PB}/api/collections/users/records/{erin}", headers=SU, json={"banned_until": ""})
r2 = pb_login("erin@example.com", "erin-password")
requests.patch(f"{PB}/api/collections/users/records/{erin}", headers=SU, json={"banned_until": ban})
check("Auth", "control: same password works once ban is lifted (hash is valid)", 200, r2.status_code)
r = pb_login("legacy@example.com", "legacy-pass-1")
lg = requests.get(f"{PB}/api/collections/users/records?filter=" + urllib.parse.quote('email="legacy@example.com"'), headers=SU).json()["items"][0]
check("Auth", "argon2 user: login fails, flagged password_reset_required", (400, True), (r.status_code, lg["password_reset_required"]))
ph = requests.get(f"{PB}/api/collections/users/records?filter=" + urllib.parse.quote('phone="84901234567"'), headers=SU).json()["items"]
check("Auth", "phone-only user imported (SMS login not available)", seed["phone"]["id"], ph[0]["id"] if ph else None)

# OAuth2 through the same IdP -> must resolve to the migrated user via _externalAuths
requests.get(f"{IDP}/control/identity", params={"sub": "kc-user-0001", "email": "oauth.user@example.com", "name": "OAuth User"})
prov = requests.get(f"{PB}/api/collections/users/auth-methods").json()["oauth2"]["providers"][0]
redirect = f"{PB}/api/oauth2-redirect"
loc = requests.get(prov["authURL"] + urllib.parse.quote(redirect), allow_redirects=False).headers["Location"]
code = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(loc).query))["code"]
r = requests.post(f"{PB}/api/collections/users/auth-with-oauth2", json={"provider": "oidc", "code": code, "codeVerifier": prov["codeVerifier"], "redirectURL": redirect}).json()
check("Auth", "OAuth login (same IdP subject) -> SAME user id, not a new account", (seed["oauth"]["id"], False), (r.get("record", {}).get("id"), r.get("meta", {}).get("isNew")))

# Session continuity: live Supabase access token -> PocketBase token
st = requests.post(f"{AUTH}/token?grant_type=password", json={"email": "dave@example.com", "password": "dave-secret!"}).json()["access_token"]
r = requests.post(f"{PB}/api/supabase/exchange", json={"access_token": st})
check("Auth", "token exchange: Supabase JWT -> PocketBase token (same user)", (200, seed["dave"]["id"]), (r.status_code, r.json().get("record", {}).get("id")))
check("Auth", "token exchange rejects forged token", 401, requests.post(f"{PB}/api/supabase/exchange", json={"access_token": st[:-4] + "AAAA"}).status_code)

# ---------- D. rules (RLS -> API rules) ----------
tok = {k: {"Authorization": pb_login(seed[k]["email"], seed[k]["password"]).json()["token"]} for k in ("alice", "bob", "dave")}
pub = q("select count(*) from public.posts where status='published'")
check("Rules", "guest lists posts -> only published", pub, requests.get(f"{PB}/api/collections/posts/records?perPage=1").json()["totalItems"])
bob_draft = q("select id::text from public.posts where title='Bob draft (private)'")
check("Rules", "guest cannot view Bob's draft", 404, requests.get(f"{PB}/api/collections/posts/records/{bob_draft}").status_code)
check("Rules", "Bob can view his own draft", 200, requests.get(f"{PB}/api/collections/posts/records/{bob_draft}", headers=tok["bob"]).status_code)
check("Rules", "Alice cannot update Bob's post", 404, requests.patch(f"{PB}/api/collections/posts/records/{bob_draft}", headers=tok["alice"], json={"title": "hacked!"}).status_code)
edge = "11111111-2222-4333-8444-555555555555"
check("Rules", "Alice updates her own post", 200, requests.patch(f"{PB}/api/collections/posts/records/{edge}", headers=tok["alice"], json={"title": "Edge case post (edited)"}).status_code)
r = requests.patch(f"{PB}/api/collections/posts/records/{edge}", headers=tok["alice"], json={"author_id": seed["bob"]["id"]})
check("Rules", "UPDATE WITH CHECK: cannot reassign post to Bob", True, r.status_code in (400, 403, 404))
r = requests.post(f"{PB}/api/collections/posts/records", headers=tok["alice"], json={"title": "Spoofed", "author_id": seed["bob"]["id"]})
check("Rules", "INSERT WITH CHECK: cannot create post as Bob", True, r.status_code in (400, 403))
r = requests.post(f"{PB}/api/collections/posts/records", headers=tok["alice"], json={"title": "Created after migration", "author_id": seed["alice"]["id"]})
check("Rules", "create own post; DEFAULT 'draft' applied by generated hook; new id is UUID-shaped", (200, "draft", 36), (r.status_code, r.json().get("status"), len(r.json().get("id", ""))))
check("Rules", "guest sees no comments (TO authenticated)", 0, requests.get(f"{PB}/api/collections/comments/records?perPage=1").json()["totalItems"])
cid = q("select c.id::text from public.comments c join public.posts p on p.id=c.post_id where p.author_id=%s and c.user_id=%s limit 1", seed["alice"]["id"], seed["dave"]["id"])
check("Rules", "EXISTS policy: post author (Alice) deletes Dave's comment", 204, requests.delete(f"{PB}/api/collections/comments/records/{cid}", headers=tok["alice"]).status_code)
cid2 = q("select c.id::text from public.comments c join public.posts p on p.id=c.post_id where p.author_id=%s and c.user_id=%s limit 1", seed["carol"]["id"], seed["bob"]["id"])
check("Rules", "Dave cannot delete Bob's comment on Carol's post", 404, requests.delete(f"{PB}/api/collections/comments/records/{cid2}", headers=tok["dave"]).status_code)
settings = f"{PB}/api/collections/user_settings/records/{seed['alice']['id']}"
check("Rules", "Alice reads her own settings (policy on the user_id key)", 200, requests.get(settings, headers=tok["alice"]).status_code)
check("Rules", "Bob cannot read Alice's settings", 404, requests.get(settings, headers=tok["bob"]).status_code)
# tables app users could not reach on Supabase stay locked (superusers only), for guests and signed-in users alike
jobs = f"{PB}/api/collections/internal_jobs/records"
check("Rules", "policy TO service_role: guest cannot list", 403, requests.get(jobs).status_code)
check("Rules", "policy TO service_role: signed-in user cannot list", 403, requests.get(jobs, headers=tok["alice"]).status_code)
check("Rules", "policy TO service_role: guest cannot create", 403, requests.post(jobs, json={"payload": "injected"}).status_code)
check("Rules", "policy TO supabase_auth_admin (RBAC guide): guest cannot list user_roles", 403, requests.get(f"{PB}/api/collections/user_roles/records").status_code)
check("Rules", "table privileges revoked from anon/authenticated: guest cannot list", 403, requests.get(f"{PB}/api/collections/ledger/records").status_code)
check("Rules", "locked tables still hold their rows", (2, 1, 1), (pb_count("internal_jobs"), pb_count("user_roles"), pb_count("ledger")))

# ---------- E. files ----------
def sb_file(bucket, name):
    return requests.get(f"{STORAGE}/object/{bucket}/{name}", headers={"Authorization": f"Bearer {SERVICE}"}).content
md5 = lambda b: hashlib.md5(b).hexdigest()
for k in ("alice", "bob"):
    u = requests.get(f"{PB}/api/collections/users/records/{seed[k]['id']}", headers=SU).json()
    f = requests.get(f"{PB}/api/files/users/{u['id']}/{u['avatar']}")
    how = "storage path" if k == "alice" else "full public URL"
    check("Files", f"{k} avatar ({how} in profiles.avatar_url) served, md5 identical", md5(sb_file("avatars", f"{seed[k]['id']}/avatar.png")), md5(f.content) if f.status_code == 200 else f.status_code)
docs = requests.get(f"{PB}/api/collections/storage_documents/records?perPage=10", headers=SU).json()["items"]
contract = [d for d in docs if d["path"].endswith("contract.pdf")][0]
url = f"{PB}/api/files/storage_documents/{contract['id']}/{contract['file']}"
check("Files", "private bucket file: guest denied", True, requests.get(url).status_code in (403, 404))
ft = lambda k: requests.post(f"{PB}/api/files/token", headers=tok[k]).json()["token"]
r = requests.get(url + "?token=" + ft("alice"))
check("Files", "private file: owner (Alice) with file token, md5 identical", md5(sb_file("documents", contract["path"])), md5(r.content) if r.status_code == 200 else r.status_code)
check("Files", "private file: Bob's file token denied", True, requests.get(url + "?token=" + ft("bob")).status_code in (403, 404))
inv = [d for d in docs if d["path"].endswith("inv-001.txt")][0]
check("Files", "nested folder path preserved in `path` field", f"{seed['alice']['id']}/2026/invoices/inv-001.txt", inv["path"])
logo = requests.get(f"{PB}/api/collections/storage_avatars/records?perPage=5", headers=SU).json()["items"][0]
check("Files", "orphan object (public bucket) served publicly", 200, requests.get(f"{PB}/api/files/storage_avatars/{logo['id']}/{logo['file']}").status_code)

json.dump(results, open(os.path.join(WORK, "verify_results.json"), "w"), indent=1, ensure_ascii=False)
print(f"\n{sum(r['ok'] for r in results)}/{len(results)} checks passed")
sys.exit(0 if all(r["ok"] for r in results) else 1)
