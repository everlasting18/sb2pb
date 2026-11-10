package app

// Supabase side: connection string checks, project ref, derived URLs, read-only sessions, readable errors.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every session sb2pb opens on the source database is read-only: Postgres itself refuses any write,
// even one caused by a bug in this tool.
const analyzeSession = `set default_transaction_read_only = on; set statement_timeout = '60s'; set lock_timeout = '5s';
set idle_in_transaction_session_timeout = '120s'; set time zone 'UTC'; set extra_float_digits = 3;
set search_path = public, extensions; set intervalstyle = 'iso_8601'`

// migrate streams whole tables, so there is no statement timeout; it is still read-only.
//   - extra_float_digits=3: Supabase sets 0, which would cut floats to 15 digits
//   - UTC so timestamptz renders with +00:00; search_path so PostGIS/pgvector functions resolve
//   - ISO 8601 intervals (P1Y2M3DT4H5M6.789S) instead of "1 year 2 mons"
const migrateSession = `set default_transaction_read_only = on; set statement_timeout = 0; set lock_timeout = '10s';
set time zone 'UTC'; set extra_float_digits = 3; set search_path = public, extensions; set intervalstyle = 'iso_8601'`

// Source describes the Supabase project behind a connection string.
type Source struct {
	DSN      string
	Config   *pgx.ConnConfig
	Ref      string // project ref, "" when it cannot be derived (self-hosted)
	Host     string
	Database string
}

var (
	reDirectHost = regexp.MustCompile(`^db\.([a-z0-9]{15,30})\.supabase\.co$`)
	reRefOnly    = regexp.MustCompile(`^[a-z0-9]{15,30}$`)
)

// sb2pb reads everything in one session (session settings, one snapshot transaction, prepared statements).
// A pooler in transaction mode hands every transaction to another server connection, so that cannot work.
const transactionPoolerHint = "this connection goes through a pooler in transaction mode (on Supabase: the Transaction pooler, port 6543), which sb2pb cannot use: it needs one session for the whole read. " +
	"Use the Session pooler string instead: Supabase Dashboard → Connect → Session pooler (port 5432)"

// ParseSource validates a connection string and works out the project ref.
func ParseSource(dsn string) (*Source, error) {
	dsn = strings.TrimSpace(dsn)
	switch {
	case dsn == "":
		return nil, errors.New("the Supabase connection string is empty")
	case strings.Contains(dsn, "[YOUR-PASSWORD]") || strings.Contains(dsn, "[YOUR_PASSWORD]"):
		return nil, errors.New("replace [YOUR-PASSWORD] in the connection string with your database password")
	case !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://"):
		return nil, errors.New("this is not a Postgres connection string (it should start with postgresql://); copy it from Supabase Dashboard → Connect")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("this is not a valid Postgres connection string: " + redactErr(err.Error(), dsn))
	}
	if host := strings.ToLower(cfg.Host); cfg.Port == 6543 && (strings.HasSuffix(host, ".supabase.com") || strings.HasSuffix(host, ".supabase.co")) {
		return nil, errors.New(transactionPoolerHint)
	}
	cfg.ConnectTimeout = 15 * time.Second
	cfg.RuntimeParams["application_name"] = "sb2pb"
	s := &Source{DSN: dsn, Config: cfg, Host: cfg.Host, Database: cfg.Database}
	if m := reDirectHost.FindStringSubmatch(strings.ToLower(cfg.Host)); m != nil {
		s.Ref = m[1]
	} else if _, ref, ok := strings.Cut(cfg.User, "."); ok && reRefOnly.MatchString(ref) {
		s.Ref = ref // pooler user: postgres.<ref>
	}
	return s, nil
}

// Redacted returns the connection string with the password hidden, for display.
func (s *Source) Redacted() string {
	u, err := url.Parse(s.DSN)
	if err != nil || u.User == nil {
		return s.Host
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "****")
	}
	return strings.Replace(u.String(), "%2A%2A%2A%2A", "****", 1)
}

func redactErr(msg, dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "****")
		}
	}
	return msg
}

// StorageURL is the Storage API base for hosted projects (https://<ref>.supabase.co/storage/v1).
func (s *Source) StorageURL() string {
	if s.Ref == "" {
		return ""
	}
	return "https://" + s.Ref + ".supabase.co/storage/v1"
}

// AuthURL is the Auth API base used by the generated token-exchange hook.
func (s *Source) AuthURL() string {
	if s.Ref == "" {
		return ""
	}
	return "https://" + s.Ref + ".supabase.co/auth/v1"
}

// Connect opens a read-only session with the given settings.
func (s *Source) Connect(ctx context.Context, session string) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, s.Config)
	if err != nil {
		return nil, errors.New(friendly(err))
	}
	if _, err := conn.Exec(ctx, session); err != nil {
		conn.Close(context.Background())
		return nil, errors.New(friendly(err))
	}
	return conn, nil
}

// friendly turns connection errors into an actionable sentence.
func friendly(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01":
			return "authentication failed: check the database password (Supabase Dashboard → Project Settings → Database → Reset password)"
		case "28000":
			return "this database user is not allowed to connect"
		case "3D000":
			return "database not found: Supabase projects use the database name \"postgres\""
		case "57014":
			return "a query was cancelled by the statement timeout"
		case "42P05", "26000": // prepared statement already exists / does not exist: statements landed on different server connections
			return transactionPoolerHint
		case "XX000":
			if strings.Contains(pgErr.Message, "Tenant or user not found") {
				return "the pooler did not recognise this project: copy the full Session pooler string from Supabase Dashboard → Connect instead of editing the host or user"
			}
		}
		return "database error: " + pgErr.Message
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout"):
		return "timed out connecting. Direct hosts (db.<ref>.supabase.co) are IPv6-only: use the Session pooler string from Supabase Dashboard → Connect"
	case strings.Contains(msg, "no such host"):
		return "host not found: check the connection string"
	case strings.Contains(msg, "refused") || strings.Contains(msg, "unreachable"):
		return "could not reach the database: use the Session pooler string (port 5432) from Supabase Dashboard → Connect"
	case strings.Contains(msg, "Tenant or user not found"):
		return "the pooler did not recognise this project: copy the full Session pooler string from Supabase Dashboard → Connect"
	}
	return "could not connect to the database: " + msg
}

// setKeyHeaders authenticates a Storage request with a Supabase secret key.
// New secret keys (sb_secret_...) are not JWTs and go only in the apikey header;
// legacy service_role keys are JWTs and are also sent as a Bearer token.
func setKeyHeaders(h interface{ Set(string, string) }, key string) {
	h.Set("apikey", key)
	if isJWT(key) {
		h.Set("Authorization", "Bearer "+key)
	}
}

func isJWT(key string) bool {
	return strings.HasPrefix(key, "eyJ") && strings.Count(key, ".") == 2
}

// checkSecretKey rejects keys that cannot read private buckets, before anything is migrated.
func checkSecretKey(key string) error {
	switch {
	case key == "":
		return nil
	case strings.HasPrefix(key, "sb_publishable_"):
		return errors.New("this is a publishable key; file copying needs a secret key (sb_secret_...) or the legacy service_role key")
	case strings.HasPrefix(key, "sb_secret_"):
		return nil
	case isJWT(key):
		if role := jwtRole(key); role != "" && role != "service_role" {
			return fmt.Errorf("this is the %q key; file copying needs a secret key (sb_secret_...) or the legacy service_role key", role)
		}
		return nil
	}
	return errors.New("this does not look like a Supabase secret key (sb_secret_...) or service_role key")
}

func jwtRole(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(payload, &claims) // not a JWT -> no role
	return claims.Role
}
