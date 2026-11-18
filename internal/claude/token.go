package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

const (
	keychainServiceBase     = "Claude Code-credentials"
	keychainAccountFallback = "claude-code-user"
	// tokenEnvBase is never read on its own: the daemon serves several
	// profiles, and one token cannot stand for two accounts.
	tokenEnvBase = "CLAUDE_CODE_OAUTH_TOKEN"
	// Exit codes of `security`: the low byte of the OSStatus.
	keychainNotFound     = 44  // errSecItemNotFound
	keychainNoGUI        = 36  // errSecInteractionNotAllowed
	keychainUserCanceled = 128 // userCanceled
	guiLessHint          = "run the report through `brishz`, whose shells are attached to the GUI " +
		"session, or set a per-profile token variable (see docs/claude_code_usage.md)"
	loginHint = "open `claude` (or run `/login` inside it) to refresh the token"
)

var keychainAccountRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// tokenInfo is claude_code_usage.py's TokenInfo.
type tokenInfo struct {
	token        string
	source       string
	expiresAt    *float64
	subscription *string
	warning      string
}

func (t *tokenInfo) expired(now float64) *bool {
	if t == nil || t.expiresAt == nil {
		return nil
	}
	v := *t.expiresAt < now
	return &v
}

// tokenError is a failed token lookup. noCredential is set when nothing
// went wrong and there simply is no credential anywhere (the signed_out
// case); a Keychain read that failed for another reason leaves it unset.
type tokenError struct {
	msg          string
	noCredential bool
}

func (e *tokenError) Error() string { return e.msg }

// keychainServiceFor derives the Keychain service the way Claude Code does:
// the base name, plus "-" and the first 8 hex digits of sha256 over the
// config dir for any non-default dir. CLAUDE_SECURESTORAGE_CONFIG_DIR, when
// set, replaces the config dir.
//
// Python NFC-normalizes the path first. The standard library has no
// normalizer; ASCII paths (the only ones seen) are unaffected.
func keychainServiceFor(configDir string, getenv func(string) (string, bool)) string {
	hashed := configDir
	if v, ok := getenv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		hashed = v
	}
	if hashed == "" {
		return keychainServiceBase
	}
	sum := sha256.Sum256([]byte(hashed))
	return keychainServiceBase + "-" + hex.EncodeToString(sum[:])[:8]
}

var nonAlnumRun = regexp.MustCompile(`[^A-Za-z0-9]+`)

// tokenEnvName is CLAUDE_CODE_OAUTH_TOKEN_<PROFILE>, the label uppercased
// with runs of non-alphanumerics collapsed to "_".
func tokenEnvName(label string) string {
	suffix := strings.ToUpper(strings.Trim(nonAlnumRun.ReplaceAllString(label, "_"), "_"))
	if suffix == "" {
		return tokenEnvBase
	}
	return tokenEnvBase + "_" + suffix
}

// keychainAccountCandidates: the passwd name of this uid first, then $USER,
// never LOGNAME (a stale LOGNAME=root once picked an orphan credential).
func (r *Reader) keychainAccountCandidates() []string {
	var names []string
	user, _ := r.getenv("USER")
	for _, n := range []string{r.passwdName(), user} {
		if n == "" || !keychainAccountRE.MatchString(n) {
			continue
		}
		dup := false
		for _, m := range names {
			dup = dup || m == n
		}
		if !dup {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return []string{keychainAccountFallback}
	}
	return names
}

func describeAccounts(accounts []string) string {
	q := make([]string, len(accounts))
	for i, a := range accounts {
		q[i] = pyStrRepr(a)
	}
	if len(accounts) == 1 {
		return "account " + strings.Join(q, ", ")
	}
	return "accounts " + strings.Join(q, ", ")
}

// userKeychainPaths names the user's keychains explicitly, login first: in
// a GUI-detached session the default search list holds only the System
// keychain, which would make every lookup look like "not found".
func (r *Reader) userKeychainPaths() []string {
	dir := filepath.Join(r.home, "Library", "Keychains")
	paths, err := filepath.Glob(filepath.Join(dir, "*.keychain-db"))
	if err != nil {
		return nil
	}
	sort.Strings(paths)
	login := filepath.Join(dir, "login.keychain-db")
	for i, p := range paths {
		if p == login {
			paths = append(paths[:i], paths[i+1:]...)
			paths = append([]string{login}, paths...)
			break
		}
	}
	return paths
}

// keychainRead is one `security` lookup: data, or an error message, or
// neither (the item is simply absent).
type keychainRead struct {
	data *value
	err  string
}

func (r *Reader) readKeychainItem(ctx context.Context, service string, account *string) keychainRead {
	argv := []string{"security", "find-generic-password", "-s", service}
	if account != nil {
		argv = append(argv, "-a", *account)
	}
	argv = append(argv, "-w")
	argv = append(argv, r.userKeychainPaths()...)
	timeout := r.keychainTimeout
	res, err := r.run.Run(ctx, procx.Cmd{Argv: argv, Timeout: timeout})
	if errors.Is(err, procx.ErrTimeout) {
		return keychainRead{err: fmt.Sprintf("timed out after %ss reading Keychain item %s "+
			"(an authorization prompt may be waiting for you)", pyG(timeout.Seconds()), pyStrRepr(service))}
	}
	if err != nil && res.ExitCode <= 0 {
		return keychainRead{err: fmt.Sprintf("could not run security: %v", err)}
	}
	switch res.ExitCode {
	case 0:
	case keychainNotFound:
		return keychainRead{}
	case keychainNoGUI:
		return keychainRead{err: fmt.Sprintf("Keychain item %s needs authorization and macOS would "+
			"not show a prompt, because this process is not attached to the "+
			"GUI session (security exit 36); %s", pyStrRepr(service), guiLessHint)}
	case keychainUserCanceled:
		return keychainRead{err: fmt.Sprintf("Keychain authorization for item %s was cancelled "+
			"(security exit 128)", pyStrRepr(service))}
	default:
		// stderr only; stdout would be the secret.
		msg := fmt.Sprintf("Keychain item %s could not be read (security exit %d)", pyStrRepr(service), res.ExitCode)
		var last string
		for _, line := range strings.Split(string(res.Stderr), "\n") {
			if strings.TrimSpace(line) != "" {
				last = strings.TrimSpace(line)
			}
		}
		if last != "" {
			msg += ": " + last
		}
		return keychainRead{err: msg}
	}
	data, perr := parseValue([]byte(strings.TrimSpace(string(res.Stdout))))
	if perr != nil {
		return keychainRead{err: fmt.Sprintf("Keychain item %s does not contain JSON", pyStrRepr(service))}
	}
	if !data.isObject() {
		return keychainRead{err: fmt.Sprintf("Keychain item %s does not contain a JSON object", pyStrRepr(service))}
	}
	return keychainRead{data: data}
}

// keychainTokenInfos returns the credentials found and, separately, every
// read that failed for a reason other than "no such item".
func (r *Reader) keychainTokenInfos(ctx context.Context, service string, accounts []*string) ([]*tokenInfo, []string) {
	if r.goos != "darwin" {
		return nil, nil
	}
	var infos []*tokenInfo
	var errs []string
	seenTokens := map[string]bool{}
	var seenAccounts []*string
	for _, acc := range accounts {
		dup := false
		for _, s := range seenAccounts {
			if (s == nil) == (acc == nil) && (s == nil || *s == *acc) {
				dup = true
			}
		}
		if dup {
			continue
		}
		seenAccounts = append(seenAccounts, acc)
		read := r.readKeychainItem(ctx, service, acc)
		if read.err != "" {
			errs = append(errs, read.err)
			continue
		}
		if read.data == nil {
			continue
		}
		source := "keychain"
		if acc != nil {
			source = "keychain:" + *acc
		}
		info := tokenInfoFromOAuth(read.data, source)
		if info == nil || seenTokens[info.token] {
			continue
		}
		seenTokens[info.token] = true
		infos = append(infos, info)
	}
	return infos, errs
}

// bestTokenInfo prefers a live credential, then the latest expiry; the
// first of equals wins, as Python's max does.
func bestTokenInfo(infos []*tokenInfo, now float64) *tokenInfo {
	var best *tokenInfo
	rank := func(t *tokenInfo) (bool, float64) {
		e := t.expired(now)
		exp := 0.0
		if t.expiresAt != nil {
			exp = *t.expiresAt
		}
		return e == nil || !*e, exp
	}
	for _, t := range infos {
		if best == nil {
			best = t
			continue
		}
		ta, tb := rank(t)
		ba, bb := rank(best)
		if (ta && !ba) || (ta == ba && tb > bb) {
			best = t
		}
	}
	return best
}

func tokenInfoFromOAuth(data *value, source string) *tokenInfo {
	tok, ok := data.getPath("claudeAiOauth", "accessToken").str()
	if !ok || tok == "" {
		return nil
	}
	info := &tokenInfo{token: tok, source: source}
	if e := data.getPath("claudeAiOauth", "expiresAt"); e.isNumber() {
		f := e.float() / 1000 // epoch milliseconds
		info.expiresAt = &f
	}
	if s, ok := data.getPath("claudeAiOauth", "subscriptionType").str(); ok {
		info.subscription = &s
	}
	return info
}

// profilePaths are one profile's local files.
type profilePaths struct {
	configDir   string // expanded; "" for the default profile
	configJSON  string
	credentials []string
}

func (r *Reader) paths(configDir string) profilePaths {
	p := profilePaths{configDir: r.expand(configDir)}
	shared := filepath.Join(r.home, ".claude", ".credentials.json")
	if p.configDir != "" {
		p.configJSON = filepath.Join(p.configDir, ".claude.json")
		own := filepath.Join(p.configDir, ".credentials.json")
		p.credentials = append(p.credentials, own)
		if own == shared {
			return p
		}
	} else {
		p.configJSON = filepath.Join(r.home, ".claude.json")
	}
	p.credentials = append(p.credentials, shared)
	return p
}

func (r *Reader) expand(p string) string {
	if p == "~" {
		return r.home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(r.home, p[2:])
	}
	return p
}

// getToken looks the profile's token up: the configured variable, the
// per-profile variable, the token file, the Keychain, the credentials files.
func (r *Reader) getToken(ctx context.Context, name, tokenEnv string, pp profilePaths) (*tokenInfo, error) {
	var envNames []string
	if tokenEnv != "" {
		envNames = append(envNames, tokenEnv)
	}
	if n := tokenEnvName(name); n != tokenEnv {
		envNames = append(envNames, n)
	}
	for _, n := range envNames {
		if v, ok := r.getenv(n); ok && v != "" {
			return &tokenInfo{token: v, source: "env:" + n}, nil
		}
	}

	// The file the shell wrapper and the Paseo watcher export from.
	tokenFile := filepath.Join(r.cfg.Claude.TokenDir, "claude-code-oauth-"+name)
	if data, err := os.ReadFile(r.expand(tokenFile)); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return &tokenInfo{token: tok, source: "token-file:" + tokenFile}, nil
		}
	}

	service := keychainServiceFor(pp.configDir, r.getenv)
	accounts := r.keychainAccountCandidates()
	account := accounts[0]
	ptrs := func(names []string) []*string {
		out := make([]*string, len(names))
		for i := range names {
			out[i] = &names[i]
		}
		return out
	}
	infos, errs := r.keychainTokenInfos(ctx, service, ptrs(accounts))
	if len(infos) == 0 && len(errs) == 0 {
		// Older Claude Code versions stored the default profile under other
		// account names, or none; "unknown" is only worth a call on the
		// unsuffixed service such a version could have written.
		legacy := append([]*string{nil}, ptrs(accounts)...)
		if pp.configDir == "" {
			unknown := "unknown"
			legacy = append(legacy, &unknown)
		}
		infos, errs = r.keychainTokenInfos(ctx, service, legacy)
	}
	now := model.Unix(r.clock())
	if info := bestTokenInfo(infos, now); info != nil {
		if expected := "keychain:" + account; info.source != expected {
			origin := "$USER"
			if account == r.passwdName() {
				origin = "this uid's passwd name"
			}
			used := "an unfiltered match"
			if info.source != "keychain" {
				_, acc, _ := strings.Cut(info.source, ":")
				used = "account " + pyStrRepr(acc)
			}
			info.warning = fmt.Sprintf("Keychain item %s has no entry for account %s (%s); used %s instead",
				pyStrRepr(service), pyStrRepr(account), origin, used)
		}
		return info, nil
	}

	for _, path := range pp.credentials {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v, err := parseValue(data)
		if err != nil || !v.isObject() {
			continue
		}
		if info := tokenInfoFromOAuth(v, "credentials-file:"+path); info != nil {
			return info, nil
		}
		break // Python stops at the first readable file
	}

	if len(errs) > 0 {
		return nil, &tokenError{msg: "could not read the Keychain credential for " +
			describeAccounts(accounts) + ": " + strings.Join(errs, "; ")}
	}
	tried := append(append([]string(nil), envNames...), tokenFile)
	return nil, &tokenError{noCredential: true, msg: fmt.Sprintf(
		"no OAuth token found (tried %s, Keychain item %s %s, and %s); %s",
		strings.Join(tried, ", "), pyStrRepr(service), describeAccounts(accounts),
		strings.Join(pp.credentials, ", "), loginHint)}
}

// keychainInfo is the `keychain` object of the legacy row.
type keychainInfo struct {
	Service string `json:"service"`
	Account string `json:"account"`
}
