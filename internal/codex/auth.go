package codex

// Auth snapshot discovery, identity and refresh-token recovery, ported from
// codex_status.py.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
)

// authClaim is the JWT claim holding the ChatGPT account details.
const authClaim = "https://api.openai.com/auth"

// authFileRE matches auth snapshot names: auth_<alias>.json, auth.<alias>.json,
// auth-<alias>.json and auth<alias>.json. The bare auth.json does not match.
var authFileRE = regexp.MustCompile(`^auth(?:\.|_|-)?(?P<alias>.+)\.json$`)

// aliasFromAuthPath is alias_from_auth_path: the regex alias, else the stem.
func aliasFromAuthPath(path string) string {
	name := filepath.Base(path)
	if m := authFileRE.FindStringSubmatch(name); m != nil {
		if a := pyStrip(m[1]); a != "" {
			return a
		}
		// The Python falls through to the stem, as an empty alias is None.
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if strings.HasPrefix(name, ".") && filepath.Ext(name) == name {
		stem = name
	}
	return pyStrip(stem)
}

// matchingFiles is iter_matching_files: regular files whose name the regex
// matches, sorted by name.
func matchingFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !isRegular(p) {
			continue
		}
		if authFileRE.MatchString(e.Name()) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return filepath.Base(out[i]) < filepath.Base(out[j]) })
	return out
}

// isRegular is Path.is_file(): a regular file, following symlinks.
func isRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func fileBytes(p string) ([]byte, bool) {
	b, err := os.ReadFile(p)
	return b, err == nil
}

// authBytesEqual is auth_bytes_equal: both readable and byte-identical.
func authBytesEqual(a, b string) bool {
	x, ok1 := fileBytes(a)
	y, ok2 := fileBytes(b)
	return ok1 && ok2 && bytes.Equal(x, y)
}

// authSources is all_auth_sources for one CODEX_HOME: every alias snapshot,
// plus the bare auth.json when its bytes match none of them.
func authSources(codexHome, home string) []authSource {
	var out []authSource
	aliasPaths := matchingFiles(codexHome)
	for _, p := range aliasPaths {
		label := aliasFromAuthPath(p)
		if label == "" {
			label = filepath.Base(p)
		}
		out = append(out, authSource{Path: p, Label: label, DisplayPath: homeRelative(p, home)})
	}
	bare := filepath.Join(codexHome, "auth.json")
	if isRegular(bare) {
		dup := false
		for _, p := range aliasPaths {
			if authBytesEqual(bare, p) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, authSource{Path: bare, Label: aliasFromAuthPath(bare), DisplayPath: homeRelative(bare, home)})
		}
	}
	return out
}

// ActiveAlias names the auth the CODEX_HOME's auth.json currently is: the
// first alias snapshot (by name) whose bytes match it, else "auth", the label
// the bare file gets. An error when auth.json cannot be read.
func ActiveAlias(codexHome string) (string, error) {
	codexHome = config.Expand(codexHome)
	bare := filepath.Join(codexHome, "auth.json")
	active, err := os.ReadFile(bare)
	if err != nil {
		return "", err
	}
	for _, p := range matchingFiles(codexHome) {
		if filepath.Base(p) == "auth.json" {
			continue
		}
		if b, ok := fileBytes(p); ok && bytes.Equal(b, active) {
			if a := aliasFromAuthPath(p); a != "" {
				return a, nil
			}
		}
	}
	return "auth", nil
}

// homeRelative is home_relative: "~/<rel>" when the resolved path is under
// the resolved home, else the path as given.
func homeRelative(path, home string) string {
	if home == "" {
		home = config.Home()
	}
	rp, err1 := filepath.EvalSymlinks(path)
	if err1 != nil {
		rp, err1 = resolveMissing(path)
	}
	rh, err2 := filepath.EvalSymlinks(home)
	if err1 == nil && err2 == nil {
		if rel, err := filepath.Rel(rh, rp); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return "~/" + rel
		}
	}
	return path
}

// resolveMissing resolves the existing prefix of a path that does not exist
// yet, as Path.resolve() (non-strict) does.
func resolveMissing(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, base := filepath.Split(abs)
	if dir == abs || dir == "" {
		return abs, nil
	}
	rd, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		rd, err = resolveMissing(filepath.Clean(dir))
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(rd, base), nil
}

// expandUser is os.path.expanduser for "~" and "~/...".
func expandUser(p, home string) string {
	if home == "" {
		home = config.Home()
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// readJSONFile is read_json_file: an object, or nil.
func readJSONFile(path string) *obj {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := parseJSON(b)
	if err != nil {
		return nil
	}
	return asObj(v)
}

// decodeJWTPayload decodes a JWT's payload without verifying it.
func decodeJWTPayload(token string) *obj {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	v, err := parseJSON(raw)
	if err != nil {
		return nil
	}
	return asObj(v)
}

// getPath is common_sub_status.get_path.
func getPath(v any, path ...string) any {
	cur := v
	for _, k := range path {
		o, ok := cur.(*obj)
		if !ok {
			return nil
		}
		cur = o.V(k)
		if emptyish(cur) {
			return nil
		}
	}
	return cur
}

// firstPath is common_sub_status.first_path.
func firstPath(v any, paths ...[]string) any {
	for _, p := range paths {
		if x := getPath(v, p...); !emptyish(x) {
			return x
		}
	}
	return nil
}

// asNonEmptyStr is common_sub_status.as_nonempty_str.
func asNonEmptyStr(v any) string {
	if emptyish(v) {
		return ""
	}
	return pyStrip(pyStr(v))
}

func p(keys ...string) []string { return keys }

// collectTokens lists id and access tokens: the explicit paths first, then a
// walk of the whole document in key order.
func collectTokens(data *obj) []string {
	var tokens []string
	seen := map[string]bool{}
	add := func(v any) {
		s, ok := v.(string)
		if !ok || s == "" || seen[s] {
			return
		}
		seen[s] = true
		tokens = append(tokens, s)
	}
	for _, path := range [][]string{p("tokens", "id_token"), p("id_token"), p("tokens", "access_token"), p("access_token")} {
		add(getPath(data, path...))
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case *obj:
			for _, k := range t.Keys() {
				if k == "id_token" || k == "access_token" {
					add(t.V(k))
				}
				walk(t.V(k))
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(data)
	return tokens
}

// readAuthClaims returns the first decodable token's claims in an auth file.
func readAuthClaims(path string) *obj {
	data := readJSONFile(path)
	if data.Len() == 0 {
		return newObj()
	}
	for _, tok := range collectTokens(data) {
		if c := decodeJWTPayload(tok); c.Len() > 0 {
			return c
		}
	}
	return newObj()
}

func authRefreshToken(data *obj) string {
	return asNonEmptyStr(firstPath(data, p("tokens", "refresh_token"), p("refresh_token")))
}

func authIDToken(data *obj) string {
	return asNonEmptyStr(firstPath(data, p("tokens", "id_token"), p("id_token")))
}

func authAccountID(data *obj) string {
	return asNonEmptyStr(firstPath(data, p("tokens", "account_id"), p("account_id")))
}

// authIdentityKey is the stable identity used to match a recovery snapshot.
func authIdentityKey(data *obj) []string {
	if data == nil {
		return nil
	}
	var claims *obj
	if tok := authIDToken(data); tok != "" {
		claims = decodeJWTPayload(tok)
	}
	auth := asObj(claims.V(authClaim))
	var parts []string
	for _, v := range []string{
		authAccountID(data),
		asNonEmptyStr(firstPath(claims, p("sub"))),
		asNonEmptyStr(firstPath(claims, p("email"), p(authClaim, "email"))),
		asNonEmptyStr(firstPath(auth, p("workspaceId"), p("workspace_id"), p("workspace", "id"), p("organization", "id"), p("default_organization", "id"))),
	} {
		if v == "" {
			continue
		}
		dup := false
		for _, x := range parts {
			if x == v {
				dup = true
			}
		}
		if !dup {
			parts = append(parts, v)
		}
	}
	return parts
}

func sameKey(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// findRecoveredAuthFile looks in backupDir for a newer auth with the same
// identity as staleFile and a different refresh token; the newest by
// (last_refresh, mtime, name) wins.
func findRecoveredAuthFile(staleFile, backupDir string) string {
	stale := readJSONFile(staleFile)
	staleKey := authIdentityKey(stale)
	staleRefresh := authRefreshToken(stale)
	if len(staleKey) == 0 {
		return ""
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return ""
	}
	var cands []string
	for _, e := range entries {
		name := e.Name()
		c := filepath.Join(backupDir, name)
		if isRegular(c) && strings.HasPrefix(name, "auth") && filepath.Ext(name) == ".json" {
			cands = append(cands, c)
		}
	}
	sort.Strings(cands)
	staleResolved, _ := filepath.EvalSymlinks(staleFile)
	type keyed struct {
		lastRefresh string
		mtime       time.Time
		name        string
		path        string
	}
	var best *keyed
	for _, c := range cands {
		if r, err := filepath.EvalSymlinks(c); err == nil && staleResolved != "" && r == staleResolved {
			continue
		}
		data := readJSONFile(c)
		if !sameKey(authIdentityKey(data), staleKey) {
			continue
		}
		cr := authRefreshToken(data)
		if staleRefresh != "" && cr != "" && cr == staleRefresh {
			continue
		}
		k := keyed{name: filepath.Base(c), path: c}
		if data != nil {
			k.lastRefresh = asNonEmptyStr(data.V("last_refresh"))
		}
		if st, err := os.Stat(c); err == nil {
			k.mtime = st.ModTime()
		}
		if best == nil || k.lastRefresh > best.lastRefresh ||
			(k.lastRefresh == best.lastRefresh && (k.mtime.After(best.mtime) ||
				(k.mtime.Equal(best.mtime) && k.name > best.name))) {
			kk := k
			best = &kk
		}
	}
	if best == nil {
		return ""
	}
	return best.path
}

// refreshTokenAlreadyUsed recognises the error that triggers recovery.
func refreshTokenAlreadyUsed(msg, stderr string) bool {
	t := strings.ToLower(msg + "\n" + stderr)
	return strings.Contains(t, "refresh token") &&
		((strings.Contains(t, "already") && strings.Contains(t, "used")) ||
			strings.Contains(t, "reuse") || strings.Contains(t, "invalid_grant"))
}

// replaceAuthFile copies source over target atomically (temp file in the
// target's directory, then rename), preserving mode and mtime as copy2
// does. It reports whether it wrote anything.
func replaceAuthFile(source, target string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, err
	}
	if _, err := os.Stat(target); err == nil && authBytesEqual(target, source) {
		return false, nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(source)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".auth.*.json")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmpName, st.Mode().Perm()); err != nil {
		return false, err
	}
	_ = os.Chtimes(tmpName, st.ModTime(), st.ModTime())
	if err := os.Rename(tmpName, target); err != nil {
		return false, err
	}
	tmpName = ""
	return true, nil
}

// copyFile is shutil.copy2 for one regular file.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, st.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// prepareCodexHome builds the isolated CODEX_HOME: the auth file as
// auth.json plus config.toml, config.json and AGENTS.md from sourceHome.
func prepareCodexHome(sourceHome, tempHome, authFile string) error {
	if err := os.MkdirAll(tempHome, 0o700); err != nil {
		return err
	}
	if err := copyFile(authFile, filepath.Join(tempHome, "auth.json")); err != nil {
		return fmt.Errorf("copying auth file: %w", err)
	}
	for _, name := range []string{"config.toml", "config.json", "AGENTS.md"} {
		src := filepath.Join(sourceHome, name)
		if isRegular(src) {
			_ = copyFile(src, filepath.Join(tempHome, name))
		}
	}
	return nil
}

// workspaceNameFromClaims picks a non-"Personal" workspace name from claims.
func workspaceNameFromClaims(claims *obj) string {
	auth := asObj(claims.V(authClaim))
	direct := asNonEmptyStr(firstPath(auth, p("workspaceName"), p("workspace_name"), p("workspace", "name"),
		p("organization", "name"), p("organization", "title"), p("default_organization", "name"), p("default_organization", "title")))
	if direct != "" && strings.ToLower(direct) != "personal" {
		return direct
	}
	orgs, ok := auth.V("organizations").([]any)
	if !ok {
		orgs, ok = claims.V("organizations").([]any)
	}
	if !ok {
		return ""
	}
	var sel *obj
	for _, o := range orgs {
		if oo, isO := o.(*obj); isO && truthy(oo.V("is_default")) {
			sel = oo
			break
		}
	}
	if sel == nil {
		for _, o := range orgs {
			if oo, isO := o.(*obj); isO {
				sel = oo
				break
			}
		}
	}
	if sel == nil {
		return ""
	}
	name := asNonEmptyStr(orChain(sel.V("title"), sel.V("name"), sel.V("display_name"), sel.V("displayName")))
	if name != "" && strings.ToLower(name) != "personal" {
		return name
	}
	return ""
}

// orChain returns the first truthy value, as a Python `a or b or c` chain.
func orChain(vs ...any) any {
	for _, v := range vs {
		if truthy(v) {
			return v
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

// buildIdentity is build_identity for a status read from an auth file.
func buildIdentity(rateLimits, accountResult, rateResult, claims *obj, authFile string) *obj {
	account := asObj(accountResult.V("account"))
	plan := asNonEmptyStr(orChain(
		rateLimits.V("planType"), rateLimits.V("plan_type"),
		firstPath(account, p("planType"), p("plan_type")),
		firstPath(claims, p(authClaim, "chatgpt_plan_type")),
	))
	email := asNonEmptyStr(orChain(
		firstPath(account, p("email")),
		firstPath(accountResult, p("email")),
		firstPath(rateResult, p("account", "email")),
		firstPath(claims, p("email"), p(authClaim, "email")),
	))
	var fileAlias any
	if authFile != "" {
		if a := aliasFromAuthPath(authFile); a != "" {
			fileAlias = a
		}
	}
	wsPaths := [][]string{p("workspaceName"), p("workspace_name"), p("workspace", "name"), p("organization", "name"), p("organization", "title")}
	var fromClaims any
	if w := workspaceNameFromClaims(claims); w != "" {
		fromClaims = w
	}
	workspace := asNonEmptyStr(orChain(
		firstPath(account, wsPaths...),
		firstPath(rateResult, wsPaths...),
		fromClaims,
		fileAlias,
	))
	id := newObj()
	if plan != "" {
		id.Set("planType", plan)
	}
	if email != "" {
		id.Set("planOwnerEmail", email)
	}
	if workspace != "" {
		id.Set("workspaceName", workspace)
	}
	return id
}
