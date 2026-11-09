# Minimal mock OIDC/Keycloak-style identity provider used for BOTH Supabase (GoTrue keycloak provider)
# and PocketBase (generic "oidc" provider). Not secure - test only.
import json, secrets, urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CURRENT = {"sub": "kc-user-0001", "email": "oauth.user@example.com", "name": "OAuth User"}
CODES, TOKENS = {}, {}
LOG = []

class H(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)

    def do_GET(self):
        u = urllib.parse.urlparse(self.path); q = dict(urllib.parse.parse_qsl(u.query))
        if u.path == "/control/identity":
            CURRENT.update({k: v for k, v in q.items() if k in ("sub", "email", "name")})
            return self._json(200, CURRENT)
        if u.path == "/control/log":
            return self._json(200, LOG)
        if u.path.endswith("/protocol/openid-connect/auth"):
            code = secrets.token_hex(8); CODES[code] = dict(CURRENT)
            LOG.append({"auth": q.get("client_id"), "redirect_uri": q.get("redirect_uri"), "sub": CURRENT["sub"]})
            loc = q["redirect_uri"] + ("&" if "?" in q["redirect_uri"] else "?") + urllib.parse.urlencode({"code": code, "state": q.get("state", "")})
            self.send_response(302); self.send_header("Location", loc); self.end_headers(); return
        if u.path.endswith("/protocol/openid-connect/userinfo"):
            tok = self.headers.get("Authorization", "").replace("Bearer ", "")
            ident = TOKENS.get(tok)
            if not ident: return self._json(401, {"error": "invalid_token"})
            return self._json(200, {**ident, "email_verified": True, "preferred_username": ident["email"].split("@")[0]})
        self._json(404, {"error": "not found"})

    def do_POST(self):
        u = urllib.parse.urlparse(self.path)
        body = self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()
        f = dict(urllib.parse.parse_qsl(body))
        if u.path.endswith("/protocol/openid-connect/token"):
            ident = CODES.pop(f.get("code"), None)
            if not ident: return self._json(400, {"error": "invalid_grant"})
            at = secrets.token_hex(16); TOKENS[at] = ident
            LOG.append({"token_exchange_for": ident["sub"]})
            return self._json(200, {"access_token": at, "token_type": "Bearer", "expires_in": 3600})
        self._json(404, {"error": "not found"})

    def log_message(self, *a): pass

ThreadingHTTPServer(("0.0.0.0", 9555), H).serve_forever()
