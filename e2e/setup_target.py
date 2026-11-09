# Prepares the e2e PocketBase instance the way a customer would before migrating:
# an app name, and a generic OIDC provider pointing at the mock IdP. Both must survive the migration.
import os, sys, requests
PB = os.environ.get("PB_URL", "http://127.0.0.1:8090")
tok = requests.post(f"{PB}/api/collections/_superusers/auth-with-password",
                    json={"identity": os.environ["PB_SUPERUSER_EMAIL"], "password": os.environ["PB_SUPERUSER_PASSWORD"]}).json()["token"]
H = {"Authorization": tok}
r = requests.patch(f"{PB}/api/settings", headers=H, json={"meta": {"appName": "Customer App · Pocketbasecloud"}})
r.raise_for_status()
idp = "http://172.17.0.1:9555/realms/demo/protocol/openid-connect"
r = requests.patch(f"{PB}/api/collections/users", headers=H, json={"oauth2": {"enabled": True,
    "mappedFields": {"name": "name", "avatarURL": "avatar"},
    "providers": [{"name": "oidc", "clientId": "demo-client", "clientSecret": "demo-secret", "displayName": "Keycloak (mock IdP)",
                   "authURL": idp + "/auth", "tokenURL": idp + "/token", "userInfoURL": idp + "/userinfo", "pkce": False}]}})
if r.status_code >= 300:
    sys.exit(f"configure oauth2: {r.status_code} {r.text}")
print("target configured: app name + oidc provider")
