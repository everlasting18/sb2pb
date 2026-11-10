package app

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Translates Supabase RLS policy expressions (as deparsed by pg_policies) into PocketBase API rule filters.
// Anything it does not understand fails -> caller locks the rule (null = superuser only). Fail closed.

type ruleCtx struct {
	table   *Table
	fieldOf func(col string) (string, bool)                       // postgres column -> pocketbase field
	relTo   func(col string) string                               // relation target table for a column ("" if none)
	fieldIn func(table, col string) (coll, field string, ok bool) // other table's column -> pocketbase collection + field
	bucket  string                                                // for storage.objects policies
	newRow  bool                                                  // UPDATE ... WITH CHECK: refer to @request.body
	review  bool                                                  // set when translation is "approximate"
	joins   int                                                   // counter for unique @collection join aliases
	// meDefault reports columns declared DEFAULT auth.uid(). In an INSERT check Postgres applies that default before
	// WITH CHECK runs; PocketBase checks the rule first and a generated hook fills the field afterwards, so the rule
	// accepts an empty value there.
	meDefault func(col string) bool
	insert    bool
}

var (
	reCast           = regexp.MustCompile(`::[a-z_]+(\[\])?`)
	reSelectUID      = regexp.MustCompile(`\(\s*SELECT auth\.uid\(\) AS uid\s*\)`)
	reWS             = regexp.MustCompile(`\s+`)
	reExists         = regexp.MustCompile(`^EXISTS \(SELECT 1 FROM (?:public\.)?(\w+) (?:AS )?(\w+) WHERE \(?\((\w+)\.(\w+) = (\w+)\.(\w+)\) AND \((\w+)\.(\w+) = auth\.uid\(\)\)\)?\)$`)
	reFolder         = regexp.MustCompile(`^\(storage\.foldername\(name\)\)\[1\] = \(?auth\.uid\(\)\)?$`)
	reEqLit          = regexp.MustCompile(`^(\w+) = '([^']*)'$`)
	reAuthUIDDefault = regexp.MustCompile(`^\(?auth\.uid\(\)\)?(::\w+)?$`)
	reCharLen        = regexp.MustCompile(`char_length\(\(?(\w+)\)?\) >= (\d+)\) AND \(char_length\(\(?(\w+)\)?\) <= (\d+)`)
)

func normalizeExpr(s string) string {
	s = reWS.ReplaceAllString(s, " ")
	s = reSelectUID.ReplaceAllString(s, "auth.uid()")
	s = strings.ReplaceAll(s, "(auth.uid())::text", "auth.uid()")
	s = reCast.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "( SELECT", "(SELECT")
	return strings.TrimSpace(s)
}

func stripParens(s string) string {
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && matchingClose(s, 0) == len(s)-1 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func matchingClose(s string, open int) int {
	depth, inStr := 0, false
	for i := open; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inStr = !inStr
		case inStr:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// split on a top-level boolean operator
func splitTop(s, op string) []string {
	var parts []string
	depth, inStr, last := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inStr = !inStr
		case inStr:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], " "+op+" "):
			parts = append(parts, strings.TrimSpace(s[last:i]))
			last = i + len(op) + 2
		}
	}
	return append(parts, strings.TrimSpace(s[last:]))
}

func (rc *ruleCtx) field(col string) (string, error) {
	if f, ok := rc.fieldOf(col); ok {
		if rc.newRow {
			return "@request.body." + f, nil
		}
		return f, nil
	}
	return "", fmt.Errorf("unknown column %q", col)
}

// translate returns a PocketBase filter, "TRUE"/"FALSE" for constants, or an error.
func (rc *ruleCtx) translate(expr string) (string, error) {
	e := stripParens(normalizeExpr(expr))
	if e == "true" {
		return "TRUE", nil
	}
	if e == "false" {
		return "FALSE", nil
	}
	if parts := splitTop(e, "OR"); len(parts) > 1 {
		var out []string
		for _, p := range parts {
			t, err := rc.translate(p)
			if err != nil {
				return "", err
			}
			if t == "TRUE" {
				return "TRUE", nil
			}
			if t != "FALSE" {
				out = append(out, "("+t+")")
			}
		}
		if len(out) == 0 {
			return "FALSE", nil
		}
		return strings.Join(out, " || "), nil
	}
	if parts := splitTop(e, "AND"); len(parts) > 1 {
		var out []string
		for _, p := range parts {
			t, err := rc.translate(p)
			if err != nil {
				return "", err
			}
			if t == "FALSE" {
				return "FALSE", nil
			}
			if t != "TRUE" {
				out = append(out, "("+t+")")
			}
		}
		if len(out) == 0 {
			return "TRUE", nil
		}
		return strings.Join(out, " && "), nil
	}
	return rc.atom(e)
}

func (rc *ruleCtx) atom(e string) (string, error) {
	if r, ok, err := rc.linkThroughTable(e); ok {
		return r, err
	}
	// auth.uid() = col  |  col = auth.uid()
	if l, r, ok := strings.Cut(e, " = "); ok {
		l, r = stripParens(l), stripParens(r)
		col := ""
		if l == "auth.uid()" {
			col = r
		} else if r == "auth.uid()" {
			col = l
		}
		col = strings.TrimPrefix(col, rc.table.Name+".")
		if col != "" && regexp.MustCompile(`^\w+$`).MatchString(col) {
			f, err := rc.field(col)
			if err != nil {
				return "", err
			}
			if rc.newRow {
				return fmt.Sprintf("%s:isset = false || %s = @request.auth.id", f, f), nil
			}
			if rc.insert && rc.meDefault != nil && rc.meDefault(col) {
				return fmt.Sprintf(`(%s = "" || %s = @request.auth.id)`, f, f), nil
			}
			return fmt.Sprintf("@request.auth.id = %s", f), nil
		}
		if l == "(auth.jwt() ->> 'email')" || l == "auth.jwt() ->> 'email'" {
			f, err := rc.field(r)
			if err != nil {
				return "", err
			}
			return "@request.auth.email = " + f, nil
		}
	}
	// storage policies: bucket literal and per-user folder convention
	if m := reEqLit.FindStringSubmatch(e); m != nil {
		if rc.bucket != "" && m[1] == "bucket_id" {
			if m[2] == rc.bucket {
				return "TRUE", nil
			}
			return "FALSE", nil
		}
		f, err := rc.field(m[1])
		if err != nil {
			return "", err
		}
		if rc.newRow {
			return fmt.Sprintf("%s:isset = false || %s = \"%s\"", f, f, m[2]), nil
		}
		return fmt.Sprintf("%s = \"%s\"", f, m[2]), nil
	}
	if rc.bucket != "" && reFolder.MatchString(e) {
		rc.review = true // folder == uid convention mapped onto the owner relation
		return "owner = @request.auth.id", nil
	}
	// EXISTS (SELECT 1 FROM other p WHERE p.id = this.fk AND p.col = auth.uid())  -> fk.col = @request.auth.id
	if m := reExists.FindStringSubmatch(e); m != nil {
		other, alias := m[1], m[2]
		var fk string
		if m[3] == alias && m[4] == "id" {
			fk = m[6]
		} else if m[5] == alias && m[6] == "id" {
			fk = m[4]
		}
		if fk != "" && m[7] == alias && rc.relTo(fk) == other {
			f, err := rc.field(fk)
			if err != nil {
				return "", err
			}
			rc.review = true
			return fmt.Sprintf("%s.%s = @request.auth.id", f, m[8]), nil
		}
	}
	return "", fmt.Errorf("untranslatable expression: %s", e)
}

// ---------- policies that join another table (memberships, team tables) ----------
//
//   EXISTS (SELECT 1 FROM memberships m WHERE m.org_id = tasks.org_id AND m.user_id = auth.uid() [AND m.role = 'owner'])
//   org_id IN (SELECT memberships.org_id FROM memberships WHERE memberships.user_id = auth.uid())
//   auth.uid() IN (SELECT m.user_id FROM memberships m WHERE m.org_id = tasks.org_id)
//
// become a PocketBase join on the other collection, with one alias so every condition applies to the same row:
//
//   @collection.memberships:m.org_id ?= org_id && @collection.memberships:m.user_id ?= @request.auth.id

var (
	reExistsFrom = regexp.MustCompile(`^EXISTS \(SELECT 1 FROM (?:public\.)?(\w+)(?: (?:AS )?(\w+))? WHERE (.+)\)$`)
	reInFrom     = regexp.MustCompile(`^(?:(\w+)\.)?(\w+) IN \(SELECT (?:(\w+)\.)?(\w+) FROM (?:public\.)?(\w+)(?: (?:AS )?(\w+))? WHERE (.+)\)$`)
	reUIDInFrom  = regexp.MustCompile(`^auth\.uid\(\) IN \(SELECT (?:(\w+)\.)?(\w+) FROM (?:public\.)?(\w+)(?: (?:AS )?(\w+))? WHERE (.+)\)$`)
	reQualCol    = regexp.MustCompile(`^(?:(\w+)\.)?(\w+)$`)
	reAliasName  = regexp.MustCompile(`\W`)
)

type joinCond struct {
	corrInner, corrOuter string            // other.X = this.Y
	uid                  string            // other.U = auth.uid()
	lits                 map[string]string // other.col = 'literal'
}

// linkThroughTable reports ok=false when e is not one of the join shapes above.
func (rc *ruleCtx) linkThroughTable(e string) (string, bool, error) {
	var other, alias, where string
	jc := joinCond{lits: map[string]string{}}
	switch {
	case reExistsFrom.MatchString(e):
		m := reExistsFrom.FindStringSubmatch(e)
		other, alias, where = m[1], m[2], m[3]
	case reUIDInFrom.MatchString(e):
		m := reUIDInFrom.FindStringSubmatch(e)
		other, alias, where = m[3], m[4], m[5]
		if q := m[1]; q != "" && q != other && q != alias {
			return "", true, fmt.Errorf("untranslatable expression: %s", e)
		}
		jc.uid = m[2]
	case reInFrom.MatchString(e):
		m := reInFrom.FindStringSubmatch(e)
		if m[1] != "" && m[1] != rc.table.Name {
			return "", false, nil
		}
		other, alias, where = m[5], m[6], m[7]
		jc.corrOuter, jc.corrInner = m[2], m[4]
	default:
		return "", false, nil
	}
	if alias == "" {
		alias = other
	}
	fail := func() (string, bool, error) { return "", true, fmt.Errorf("untranslatable expression: %s", e) }
	inner := func(q string) bool { return q == "" || q == alias || q == other }
	for _, c := range splitTop(stripParens(where), "AND") {
		c = stripParens(c)
		l, r, ok := strings.Cut(c, " = ")
		if !ok {
			return fail()
		}
		l, r = stripParens(l), stripParens(r)
		if r == "auth.uid()" || l == "auth.uid()" {
			col := l
			if l == "auth.uid()" {
				col = r
			}
			m := reQualCol.FindStringSubmatch(col)
			if m == nil || !inner(m[1]) || jc.uid != "" {
				return fail()
			}
			jc.uid = m[2]
			continue
		}
		if lit := reEqLit.FindStringSubmatch(c); lit != nil {
			jc.lits[lit[1]] = lit[2]
			continue
		}
		if m := reQualCol.FindStringSubmatch(l); m != nil {
			if lit := regexp.MustCompile(`^'([^']*)'$`).FindStringSubmatch(r); lit != nil && inner(m[1]) {
				jc.lits[m[2]] = lit[1]
				continue
			}
		}
		ml, mr := reQualCol.FindStringSubmatch(l), reQualCol.FindStringSubmatch(r)
		if ml == nil || mr == nil || jc.corrInner != "" {
			return fail()
		}
		switch {
		case ml[1] != "" && inner(ml[1]) && mr[1] == rc.table.Name:
			jc.corrInner, jc.corrOuter = ml[2], mr[2]
		case mr[1] != "" && inner(mr[1]) && ml[1] == rc.table.Name:
			jc.corrInner, jc.corrOuter = mr[2], ml[2]
		default:
			return fail()
		}
	}
	if jc.uid == "" || jc.corrInner == "" || rc.fieldIn == nil {
		return fail()
	}
	coll, xField, ok1 := rc.fieldIn(other, jc.corrInner)
	_, uField, ok2 := rc.fieldIn(other, jc.uid)
	if !ok1 || !ok2 {
		return fail()
	}
	yField, err := rc.field(jc.corrOuter)
	if err != nil {
		return "", true, err
	}
	if xField == "id" && rc.relTo(jc.corrOuter) == other && len(jc.lits) == 0 {
		// the other row is the one this row points to: follow the relation (same output as before)
		rc.review = true
		return fmt.Sprintf("%s.%s = @request.auth.id", yField, uField), true, nil
	}
	// every EXISTS gets its own join alias: conditions inside one EXISTS share a row, separate EXISTS do not
	rc.joins++
	a := fmt.Sprintf("rls_%s%d", reAliasName.ReplaceAllString(alias, "_"), rc.joins)
	conds := []string{
		fmt.Sprintf("@collection.%s:%s.%s ?= %s", coll, a, xField, yField),
		fmt.Sprintf("@collection.%s:%s.%s ?= @request.auth.id", coll, a, uField),
	}
	keys := make([]string, 0, len(jc.lits))
	for k := range jc.lits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, f, ok := rc.fieldIn(other, k)
		if !ok {
			return fail()
		}
		conds = append(conds, fmt.Sprintf("@collection.%s:%s.%s ?= \"%s\"", coll, a, f, jc.lits[k]))
	}
	rule := strings.Join(conds, " && ")
	if rc.newRow { // UPDATE ... WITH CHECK: only constrains the column when the request changes it
		rule = fmt.Sprintf("%s:isset = false || (%s)", yField, rule)
	}
	return rule, true, nil
}

type RuleResult struct {
	Cmd     string   `json:"cmd"`
	Rule    string   `json:"rule"`
	Status  string   `json:"status"` // translated | review | locked
	Sources []string `json:"policies,omitempty"`
	Err     string   `json:"reason,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// policyAudience reports which app requests a policy applies to. Requests from an app run as one of two Postgres
// roles: "anon" without a session and "authenticated" with one ("public" covers both). A policy for any other role
// (service_role, supabase_auth_admin, postgres, a custom role) never gave app users access, so it must not become
// a PocketBase rule: those roles are returned in other.
func policyAudience(roles []string) (anon, authed bool, other []string) {
	if len(roles) == 0 { // pg_policies always names at least "public"
		return true, true, nil
	}
	for _, r := range roles {
		switch r {
		case "public":
			anon, authed = true, true
		case "anon":
			anon = true
		case "authenticated":
			authed = true
		default:
			other = append(other, r)
		}
	}
	return anon, authed, other
}

// buildRules combines the permissive policies of one table into PocketBase rules.
func buildRules(rc *ruleCtx, rls bool, pols []Policy) map[string]*RuleResult {
	out := map[string]*RuleResult{}
	for _, cmd := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		res := &RuleResult{Cmd: cmd, Status: "locked"}
		out[cmd] = res
		rc.review = false
		if !rls {
			res.Err = "RLS disabled on source table"
			continue
		}
		// a policy only matters to a role that also holds the table privilege (GRANT)
		grantAnon, grantAuthed := rc.table.can("anon", cmd), rc.table.can("authenticated", cmd)
		if !grantAnon && !grantAuthed {
			res.Err = "anon and authenticated have no " + cmd + " privilege on the table (no GRANT)"
			continue
		}
		var parts, skipped []string
		failed := false
		for _, p := range pols {
			if p.Cmd != cmd && p.Cmd != "ALL" {
				continue
			}
			if p.Permissive != "PERMISSIVE" {
				failed, res.Err = true, "restrictive policies not supported yet"
				break
			}
			forAnon, forAuthed, _ := policyAudience(p.Roles)
			forAnon, forAuthed = forAnon && grantAnon, forAuthed && grantAuthed
			if !forAnon && !forAuthed { // e.g. TO service_role: app users never passed through this policy
				skipped = append(skipped, fmt.Sprintf("%q (%s)", p.Name, strings.Join(p.Roles, ", ")))
				continue
			}
			res.Sources = append(res.Sources, p.Name)
			rc.newRow = false
			var expr string
			var err error
			switch cmd {
			case "INSERT":
				rc.insert = true
				expr, err = rc.translate(p.Check)
				rc.insert = false
			case "UPDATE":
				expr, err = rc.translate(p.Qual)
				if err == nil && p.Check != "" {
					rc.newRow = true
					var chk string
					chk, err = rc.translate(p.Check)
					rc.newRow = false
					if err == nil && chk != "TRUE" {
						expr = fmt.Sprintf("(%s) && (%s)", expr, chk)
					}
				}
			default:
				expr, err = rc.translate(p.Qual)
			}
			if err != nil {
				failed, res.Err = true, err.Error()
				break
			}
			roleGuard := ""
			if !forAnon {
				roleGuard = `@request.auth.id != ""`
			} else if !forAuthed {
				roleGuard = `@request.auth.id = ""`
			}
			switch {
			case expr == "FALSE":
				continue
			case expr == "TRUE" && roleGuard == "":
				parts = append(parts, "TRUE")
			case expr == "TRUE":
				parts = append(parts, roleGuard)
			case roleGuard != "":
				parts = append(parts, roleGuard+" && ("+expr+")")
			default:
				parts = append(parts, expr)
			}
		}
		if len(skipped) > 0 {
			res.Note = "not used, they apply to roles or privileges app users do not have: " + strings.Join(skipped, ", ")
		}
		if failed || len(parts) == 0 {
			if !failed && res.Err == "" {
				res.Err = "no policy for this command (Postgres denies by default)"
				if len(skipped) > 0 {
					res.Err = "no policy for app users (anon, authenticated) on this command; only for other roles: " + strings.Join(skipped, ", ")
				}
			}
			continue
		}
		rule := ""
		for _, p := range parts {
			if p == "TRUE" {
				rule = "" // public
				goto done
			}
		}
		if len(parts) == 1 {
			rule = parts[0]
		} else {
			rule = "(" + strings.Join(parts, ") || (") + ")"
		}
	done:
		res.Rule = rule
		res.Status = "translated"
		if rc.review {
			res.Status = "review"
		}
	}
	return out
}
