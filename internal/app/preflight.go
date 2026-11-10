package app

// Schemas sb2pb cannot migrate yet. They are found during the read-only analysis, so `analyze` reports them and
// `migrate` stops before the PocketBase instance is touched, instead of failing halfway with a validation error.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// PocketBase collection and field names: ASCII letters, digits and underscore.
var rePBName = regexp.MustCompile(`^\w+$`)

// builtinCollections are the names a fresh PocketBase instance already uses (lower case; names ignore case).
var builtinCollections = map[string]bool{"users": true, "_superusers": true, "_mfas": true, "_otps": true,
	"_externalauths": true, "_authorigins": true, "_collections": true, "_params": true, "_migrations": true}

// badPBName says why PocketBase would refuse a name, or "" when it is fine.
func badPBName(name string, maxLen int) string {
	switch {
	case !rePBName.MatchString(name):
		return "only letters, digits and _ are allowed"
	case len(name) > maxLen:
		return fmt.Sprintf("longer than %d characters", maxLen)
	case strings.Contains(name, "_via_"):
		return `"_via_" is reserved`
	}
	return ""
}

// schemaStoppers lists what makes the planned collections impossible to create. Each item stops `migrate`.
func schemaStoppers(plans []*CollPlan) []RItem {
	const unchanged = " Nothing is changed on PocketBase until this is fixed."
	var badTables, badCols, builtin, sameName []string
	seen := map[string]string{} // lower-case collection name -> table
	for _, cp := range plans {
		if !cp.Users {
			if why := badPBName(cp.Name, 255); why != "" {
				badTables = append(badTables, fmt.Sprintf("`%s` (%s)", cp.PGName, why))
			}
			key := strings.ToLower(cp.Name)
			switch other, dup := seen[key]; {
			case builtinCollections[key]:
				builtin = append(builtin, "`"+cp.PGName+"`")
			case dup:
				sameName = append(sameName, fmt.Sprintf("`%s` / `%s`", other, cp.PGName))
			default:
				seen[key] = cp.PGName
			}
		}
		fields := map[string]string{} // lower-case field name -> column
		for _, fp := range cp.Fields {
			if fp.Kind == "skip" || fp.PGName == "" {
				continue
			}
			ref := cp.PGName + "." + fp.PGName
			if why := badPBName(fp.Name, 100); why != "" && fp.Kind != "id" {
				badCols = append(badCols, fmt.Sprintf("`%s` (%s)", ref, why))
				continue
			}
			key := strings.ToLower(fp.Name)
			if other, dup := fields[key]; dup {
				badCols = append(badCols, fmt.Sprintf("`%s` (same name as `%s` once case is ignored)", ref, other))
				continue
			}
			fields[key] = fp.PGName
		}
	}
	var out []RItem
	stop := func(title, detail, handling string) {
		out = append(out, RItem{Severity: "blocker", Area: "Schema", Title: title, Detail: detail, Handling: handling + unchanged, Penalty: 30, Stops: true})
	}
	if len(badTables) > 0 {
		sort.Strings(badTables)
		stop("Table names PocketBase cannot use: "+strings.Join(badTables, ", "),
			"A PocketBase collection name may only contain letters, digits and `_`. sb2pb does not rename tables yet.",
			"Rename the table on Supabase (`alter table \"Old Name\" rename to old_name`) and run sb2pb again.")
	}
	if len(builtin) > 0 {
		sort.Strings(builtin)
		stop("Tables named like a built-in PocketBase collection: "+strings.Join(builtin, ", "),
			"PocketBase already has a collection with this name. A table called `users` is only merged into it when its primary key references `auth.users`.",
			"Rename the table on Supabase (for example `app_users`) and run sb2pb again.")
	}
	if len(sameName) > 0 {
		sort.Strings(sameName)
		stop("Table names that differ only in upper/lower case: "+strings.Join(sameName, ", "),
			"PocketBase collection names ignore case, so both tables would become the same collection.",
			"Rename one of the tables on Supabase and run sb2pb again.")
	}
	if len(badCols) > 0 {
		sort.Strings(badCols)
		stop("Column names PocketBase cannot use: "+strings.Join(badCols, ", "),
			"A PocketBase field name may only contain letters, digits and `_`, and must be unique in its collection whatever the case.",
			"Rename the column on Supabase (`alter table t rename column \"Old Name\" to old_name`) and run sb2pb again.")
	}
	return out
}

// Stoppers returns the items that make a migration impossible; --allow-blockers does not override them.
func (r *Readiness) Stoppers() []RItem {
	var out []RItem
	for _, it := range r.Items {
		if it.Stops {
			out = append(out, it)
		}
	}
	return out
}
