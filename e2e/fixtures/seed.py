# Seeds the e2e Supabase stack through its real APIs: users (bcrypt, argon2, MFA, anonymous, phone, OAuth), files, rows.
import json, io, os, time, urllib.parse, requests, pyotp, psycopg
from PIL import Image, ImageDraw
from argon2 import PasswordHasher

AUTH = "http://localhost:9999"; STORAGE = "http://localhost:5000"; IDP = "http://172.17.0.1:9555"
E2E = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WORK = os.environ.get("E2E_WORK", os.path.join(E2E, ".work"))
SERVICE = open(os.path.join(E2E, "supabase", "demo.env")).read().split("SERVICE_ROLE_KEY=")[1].split("\n")[0]
ADM = {"Authorization": f"Bearer {SERVICE}", "apikey": SERVICE}
PG = "postgresql://postgres:your-super-secret-and-long-postgres-password@localhost:54322/postgres"
out = {}

def signup(email, pw, meta):
    r = requests.post(f"{AUTH}/signup", json={"email": email, "password": pw, "data": meta}); r.raise_for_status()
    j = r.json(); return j["user"]["id"], j["access_token"]

users = {
  "alice": ("alice@example.com", "alice-pass-2026", {"full_name": "Alice Nguyễn"}),
  "bob":   ("bob@example.com",   "bob12345",        {"full_name": "Bob Trần"}),
  "carol": ("carol@example.com", "carol6",          {"full_name": "Carol Lê"}),   # 6-char pw (Supabase min=6, PocketBase min=8)
  "dave":  ("dave@example.com",  "dave-secret!",    {"full_name": "Dave Phạm"}),
  "erin":  ("erin@example.com",  "erin-password",   {"full_name": "Erin Võ"}),
}
for k, (e, p, m) in users.items():
    uid, tok = signup(e, p, m); out[k] = {"id": uid, "email": e, "password": p, "token": tok}
print("email users:", {k: v["id"] for k, v in out.items()})

# TOTP MFA for bob
h = {"Authorization": f"Bearer {out['bob']['token']}"}
f = requests.post(f"{AUTH}/factors", headers=h, json={"factor_type": "totp", "friendly_name": "bob phone"}).json()
ch = requests.post(f"{AUTH}/factors/{f['id']}/challenge", headers=h).json()
v = requests.post(f"{AUTH}/factors/{f['id']}/verify", headers=h, json={"challenge_id": ch["id"], "code": pyotp.TOTP(f["totp"]["secret"]).now()})
print("bob TOTP verify:", v.status_code)

# anonymous user
a = requests.post(f"{AUTH}/signup", json={}).json(); out["anon"] = {"id": a["user"]["id"]}
print("anonymous:", a["user"]["id"], a["user"].get("is_anonymous"))

# phone-only user (SMS OTP, test OTP configured)
requests.post(f"{AUTH}/otp", json={"phone": "84901234567"}).raise_for_status()
ph = requests.post(f"{AUTH}/verify", json={"type": "sms", "phone": "84901234567", "token": "123456"}).json()
out["phone"] = {"id": ph["user"]["id"]}; print("phone user:", ph["user"]["id"])

# OAuth user via keycloak provider -> mock IdP (real GoTrue OAuth flow)
requests.get(f"{IDP}/control/identity", params={"sub": "kc-user-0001", "email": "oauth.user@example.com", "name": "OAuth User"})
s = requests.Session()
r = s.get(f"{AUTH}/authorize", params={"provider": "keycloak", "redirect_to": "http://localhost:3000"}, allow_redirects=False)
r = s.get(r.headers["Location"], allow_redirects=False)            # IdP -> back to GoTrue /callback
r = s.get(r.headers["Location"], allow_redirects=False)            # GoTrue callback -> site url #access_token=
frag = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(r.headers["Location"]).fragment))
me = requests.get(f"{AUTH}/user", headers={"Authorization": f"Bearer {frag['access_token']}"}).json()
out["oauth"] = {"id": me["id"], "email": me["email"], "token": frag["access_token"]}
print("oauth user:", me["id"], [i["provider"] for i in me["identities"]])

# legacy user imported into Supabase with an argon2id hash (GoTrue supports it)
ah = PasswordHasher().hash("legacy-pass-1")
r = requests.post(f"{AUTH}/admin/users", headers=ADM, json={"email": "legacy@example.com", "password_hash": ah, "email_confirm": True})
print("argon2 user create:", r.status_code, r.text[:120] if r.status_code >= 300 else "")
r = requests.post(f"{AUTH}/token?grant_type=password", json={"email": "legacy@example.com", "password": "legacy-pass-1"})
print("argon2 user login on Supabase:", r.status_code)
out["legacy"] = {"email": "legacy@example.com", "password": "legacy-pass-1"}

# ban erin
r = requests.put(f"{AUTH}/admin/users/{out['erin']['id']}", headers=ADM, json={"ban_duration": "87600h"}); print("ban erin:", r.status_code)

# storage uploads (as the users themselves -> owner set)
def png(text, color):
    im = Image.new("RGB", (256, 256), color); d = ImageDraw.Draw(im); d.ellipse((48, 48, 208, 208), fill="white")
    d.text((100, 120), text, fill=color); b = io.BytesIO(); im.save(b, "PNG"); return b.getvalue()
def up(bucket, path, data, ctype, token):
    r = requests.post(f"{STORAGE}/object/{bucket}/{path}", headers={"Authorization": f"Bearer {token}", "Content-Type": ctype}, data=data)
    print("upload", bucket, path, r.status_code, r.text[:100] if r.status_code >= 300 else ""); r.raise_for_status()
for k, col in (("alice", "#e4572e"), ("bob", "#2e86ab")):
    up("avatars", f"{out[k]['id']}/avatar.png", png(k.upper(), col), "image/png", out[k]["token"])
pdf = io.BytesIO(); Image.new("RGB", (595, 842), "white").save(pdf, "PDF")
up("documents", f"{out['alice']['id']}/contract.pdf", pdf.getvalue(), "application/pdf", out["alice"]["token"])
up("documents", f"{out['alice']['id']}/2026/invoices/inv-001.txt", b"Invoice 001 - 1.000.000 VND", "text/plain", out["alice"]["token"])
up("avatars", "brand/logo.png", png("PB", "#333333"), "image/png", SERVICE)   # orphan object, owner null

with psycopg.connect(PG, autocommit=True) as c:
    a_id, b_id = out["alice"]["id"], out["bob"]["id"]
    c.execute("update public.profiles set username=%s, website=%s, birthday=%s, avatar_url=%s, phone=%s, updated_at=now() where id=%s",
              ("alice", "https://alice.dev", "1995-04-12", f"{a_id}/avatar.png", "0900 000 001", a_id))
    c.execute("insert into public.user_settings (user_id, theme, locale) values (%s, 'dark', 'vi'), (%s, 'light', null)", (a_id, b_id))
    c.execute("insert into public.user_roles (user_id, role) values (%s, 'admin')", (a_id,))
    c.execute("update public.profiles set username=%s, avatar_url=%s where id=%s",
              ("bob", f"http://localhost:8000/storage/v1/object/public/avatars/{b_id}/avatar.png", b_id))
    for k in ("carol", "dave", "erin", "oauth"):
        c.execute("update public.profiles set username=%s where id=%s", (k, out[k]["id"]))
    authors = [out[k]["id"] for k in ("alice", "bob", "carol", "dave", "erin", "oauth")]
    # edge-case post
    c.execute("""insert into public.posts (id, author_id, title, body, status, tags, metadata, views, big_counter, rating, is_featured, published_at, created_at)
                 values ('11111111-2222-4333-8444-555555555555', %s, 'Edge case post', 'Chào PocketBase 👋', 'published', '{sqlite,migrate}',
                 '{"lang":"vi","nested":{"a":[1,2,3]}}', 42, 9007199254740993, 4.5, null, '2024-02-03 04:05:06.123456+07', '2024-02-01 10:00:00+00')""", (a_id,))
    c.execute("insert into public.posts (author_id, title, status) values (%s, 'Bob draft (private)', 'draft')", (b_id,))
    # bulk data
    c.execute("""insert into public.posts (author_id, title, body, status, tags, metadata, views, rating, is_featured, published_at, created_at)
      select (%s::uuid[])[1 + (g %% 6)], 'Post #' || g, md5(g::text), (array['draft','published','archived']::post_status[])[1 + (g %% 3)],
             array['tag' || (g %% 10)], jsonb_build_object('n', g), (g * 7) %% 1000, ((g %% 50) / 10.0)::numeric(3,1),
             case when g %% 3 = 0 then null else g %% 2 = 0 end,
             now() - (g || ' minutes')::interval, now() - (g || ' minutes')::interval
      from generate_series(1, 20000) g""", (authors,))
    c.execute("""insert into public.comments (post_id, user_id, content, created_at)
      select p.id, (%s::uuid[])[1 + (g %% 6)], 'comment ' || g, now() - (g || ' seconds')::interval
      from generate_series(1, 100000) g join lateral (select id from public.posts order by id offset (g %% 20000) limit 1) p on true""", (authors,))
    c.execute("""insert into public.post_likes (post_id, user_id)
      select p.id, u from (select id, row_number() over () rn from public.posts) p, unnest(%s::uuid[]) u where p.rn %% 4 = 0 on conflict do nothing""", (authors,))
    for t in ("auth.users", "auth.identities", "auth.mfa_factors", "public.profiles", "public.posts", "public.comments", "public.post_likes", "storage.objects"):
        print(t, c.execute(f"select count(*) from {t}").fetchone()[0])

json.dump(out, open(os.path.join(WORK, "seed_out.json"), "w"), indent=1)
