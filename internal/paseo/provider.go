package paseo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Identity is what a Paseo provider resolves to for quota purposes: the
// native agent, the quota profile, the native home and a hash of the launch
// configuration (labels excluded).
type Identity struct {
	Agent      string // "claude", "codex" or "agy"
	Profile    string // Claude profile name, "codex", or the agy account
	NativeHome string // resolved CLAUDE_CONFIG_DIR, CODEX_HOME or agy app data dir
	ConfigHash string
}

// providers reads agents.providers from <home>/config.json; a missing file
// means no providers.
func providers(home string) (*Object, error) {
	file := filepath.Join(home, "config.json")
	if _, err := os.Stat(file); err != nil {
		return NewObject(), nil
	}
	cfg, err := readObject(file)
	if err != nil {
		return nil, err
	}
	agents := NewObject()
	if v, ok := cfg.Get("agents"); ok {
		o, isObj := v.(*Object)
		if !isObj {
			return nil, errors.New("config.json: agents is not an object")
		}
		agents = o
	}
	if v, ok := agents.Get("providers"); ok {
		o, isObj := v.(*Object)
		if !isObj {
			return nil, errors.New("config.json: agents.providers is not an object")
		}
		return o, nil
	}
	return NewObject(), nil
}

// envOf returns entry["env"] as an object; absent means empty.
func envOf(entry *Object) (*Object, error) {
	v, ok := entry.Get("env")
	if !ok {
		return NewObject(), nil
	}
	o, isObj := v.(*Object)
	if !isObj {
		return nil, errors.New("provider env is not an object")
	}
	return o, nil
}

// mergeUnder adds the keys of low that high lacks: Python's {**low, **high}
// as far as values go (key order does not matter to the digest).
func mergeUnder(high, low *Object, skip ...string) {
next:
	for _, k := range low.Keys() {
		for _, s := range skip {
			if k == s {
				continue next
			}
		}
		if _, ok := high.Get(k); !ok {
			high.Set(k, low.Value(k))
		}
	}
}

// providerConfig ports provider_config: follow the extends chain to claude or
// codex, merge launch settings (labels never count), and name the quota
// identity. A chain that ends elsewhere is an Antigravity provider when
// agyAccount says so (see agyIdentity), else refused.
func (e *Engine) providerConfig(home string, provider any) (Identity, error) {
	provs, err := providers(home)
	if err != nil {
		return Identity{}, err
	}
	result, env := NewObject(), NewObject()
	seen := map[string]bool{}
	current := provider
	for {
		name, ok := current.(string)
		if ok && (name == "claude" || name == "codex") {
			break
		}
		entry, isObj := provs.Object(name)
		if !ok || seen[name] || !isObj {
			if account, isAgy, err := agyAccount(home, provider); err == nil && isAgy {
				return e.agyIdentity(home, provider, account)
			}
			return Identity{}, refuse("Provider must resolve to Claude, Codex or Antigravity")
		}
		seen[name] = true
		entryEnv, err := envOf(entry)
		if err != nil {
			return Identity{}, err
		}
		mergeUnder(result, entry, "label", "extends", "env")
		mergeUnder(env, entryEnv)
		current = entry.Value("extends")
	}
	agent := current.(string)
	if v, ok := provs.Get(agent); ok {
		base, isObj := v.(*Object)
		if !isObj {
			return Identity{}, refuse("Invalid base provider configuration")
		}
		baseEnv, err := envOf(base)
		if err != nil {
			return Identity{}, err
		}
		mergeUnder(result, base, "label", "env")
		mergeUnder(env, baseEnv)
	}
	result.Set("env", env)

	key, def := "CODEX_HOME", ".codex"
	if agent == "claude" {
		key, def = "CLAUDE_CONFIG_DIR", ".claude"
	}
	explicit := ""
	if v := env.Value(key); truthy(v) {
		s, ok := v.(string)
		if !ok {
			return Identity{}, errors.New(key + " is not a string")
		}
		explicit = s
	}
	userHome := e.userHome()
	raw := explicit
	if raw == "" {
		raw = filepath.Join(userHome, def)
	}
	native := realpath(expandUser(raw, userHome))
	profile := "codex"
	if agent == "claude" {
		profile = e.claudeProfile(native, explicit != "")
		if profile == "" {
			return Identity{}, refuse("Claude quota watcher currently supports default and work profiles")
		}
	}
	hash, err := Digest(result)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Agent: agent, Profile: profile, NativeHome: native, ConfigHash: hash}, nil
}

// claudeProfile maps a resolved Claude config dir to a configured profile.
// A profile whose config_dir is set matches an explicit CLAUDE_CONFIG_DIR,
// and the profile without one matches an unset variable (they can use
// different Keychain entries); then any profile whose directory resolves to
// the same place; then the old fixed mapping (~/.claude is default,
// ~/.claude-work is work). "" means no profile.
func (e *Engine) claudeProfile(native string, explicit bool) string {
	userHome := e.userHome()
	dirOf := func(configDir string) string {
		if configDir == "" {
			configDir = filepath.Join(userHome, ".claude")
		}
		return realpath(expandUser(configDir, userHome))
	}
	var profiles []struct{ Name, ConfigDir string }
	if e.Config != nil {
		for _, p := range e.Config.Claude.Profiles {
			profiles = append(profiles, struct{ Name, ConfigDir string }{p.Name, p.ConfigDir})
		}
	}
	for _, p := range profiles {
		if (p.ConfigDir != "") == explicit && dirOf(p.ConfigDir) == native {
			return p.Name
		}
	}
	for _, p := range profiles {
		if dirOf(p.ConfigDir) == native {
			return p.Name
		}
	}
	switch native {
	case realpath(filepath.Join(userHome, ".claude")):
		return "default"
	case realpath(filepath.Join(userHome, ".claude-work")):
		return "work"
	}
	return ""
}

// binding ports binding(): the identity plus the provider and cwd that
// `paseo inspect` reports. Key order matches the Python dict.
func (e *Engine) binding(snapshot *Object, home string) (*Object, error) {
	provider, ok := snapshot.Get("Provider")
	if !ok {
		return nil, errors.New("inspect output lacks Provider")
	}
	cwd, ok := snapshot.Get("Cwd")
	if !ok {
		return nil, errors.New("inspect output lacks Cwd")
	}
	id, err := e.providerConfig(home, provider)
	if err != nil {
		return nil, err
	}
	return Obj("agent", id.Agent, "profile", id.Profile, "native_home", id.NativeHome,
		"config_hash", id.ConfigHash, "provider", provider, "cwd", cwd), nil
}

// identityOf reads the identity fields back from a stored binding.
func identityOf(binding *Object) Identity {
	s := func(k string) string { v, _ := binding.String(k); return v }
	return Identity{Agent: s("agent"), Profile: s("profile"), NativeHome: s("native_home"), ConfigHash: s("config_hash")}
}

// agyAccount ports agy_account: the Antigravity account an agent of this
// provider runs on, or ok=false when the provider is not Antigravity.
func agyAccount(home string, provider any) (string, bool, error) {
	provs, err := providers(home)
	if err != nil {
		return "", false, err
	}
	env := NewObject()
	commandName := ""
	haveCommand := false
	current := provider
	seen := map[string]bool{}
	for {
		name, ok := current.(string)
		if !ok || seen[name] {
			break
		}
		entry, isObj := provs.Object(name)
		if !isObj {
			break
		}
		seen[name] = true
		entryEnv, err := envOf(entry)
		if err != nil {
			return "", false, err
		}
		mergeUnder(env, entryEnv)
		if argv, ok := entry.Value("command").([]any); !haveCommand && ok && len(argv) > 0 {
			commandName = pathName(pyStr(argv[0]))
			haveCommand = true
		}
		current = entry.Value("extends")
	}
	cur, _ := current.(string)
	prov, _ := provider.(string)
	if cur != "antigravity" && prov != "antigravity" && !(haveCommand && commandName == "agy") {
		return "", false, nil
	}
	account := "m"
	if v := env.Value("AGY_ACCOUNT_ID"); truthy(v) {
		account = pyStr(v)
	}
	if account == "acc1" {
		account = "m"
	}
	return account, true, nil
}

// ModelFamily is model_family: "claude-opus-5-5" gives "opus", "gpt-6.1" "gpt".
func ModelFamily(model string) string {
	model = strings.TrimPrefix(model, "claude-")
	first, _, _ := strings.Cut(model, "-")
	return strings.ToLower(first)
}
