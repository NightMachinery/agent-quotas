// Package launchd installs the daemon as a per-user launchd agent.
package launchd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// Label is the launchd job label.
const Label = "io.github.nightmachinery.agent-quotas"

// Spec is what goes into the plist.
type Spec struct {
	Label   string
	Binary  string
	LogPath string
	Path    string
	Home    string
}

var plist = template.Must(template.New("plist").Funcs(template.FuncMap{"x": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{x .Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{x .Binary}}</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>WorkingDirectory</key>
	<string>{{x .Home}}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{x .Path}}</string>
	</dict>
	<key>StandardOutPath</key>
	<string>{{x .LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{x .LogPath}}</string>
</dict>
</plist>
`))

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Render returns the plist for spec.
func Render(spec Spec) (string, error) {
	var b bytes.Buffer
	err := plist.Execute(&b, spec)
	return b.String(), err
}

// Tools are the programs the daemon runs; their directories go on its PATH.
var Tools = []string{"codex", "agy", "paseo", "tmux", "kitty", "hs", "agent_session", "claude", "security", "osascript"}

// SearchPath builds the daemon's PATH: the directories of the tools found on
// the installing shell's PATH, then the usual system directories.
func SearchPath(binary string, lookPath func(string) (string, error), home string) string {
	var dirs []string
	add := func(d string) {
		if d == "" {
			return
		}
		for _, e := range dirs {
			if e == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	add(filepath.Dir(binary))
	found := map[string]bool{}
	for _, t := range Tools {
		if p, err := lookPath(t); err == nil {
			found[filepath.Dir(p)] = true
		}
	}
	extra := make([]string, 0, len(found))
	for d := range found {
		extra = append(extra, d)
	}
	sort.Strings(extra)
	for _, d := range extra {
		add(d)
	}
	for _, d := range []string{filepath.Join(home, "go", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		add(d)
	}
	return strings.Join(dirs, ":")
}

// PlistPath is where the agent's plist lives.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// Install writes the plist and (re)loads the job.
func Install(ctx context.Context, run procx.Runner, spec Spec, home string) (string, error) {
	text, err := Render(spec)
	if err != nil {
		return "", err
	}
	path := PlistPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, _ = run.Run(ctx, procx.Cmd{Argv: []string{"launchctl", "bootout", domain + "/" + Label}, Timeout: 20 * time.Second})
	if _, err := run.Run(ctx, procx.Cmd{Argv: []string{"launchctl", "bootstrap", domain, path}, Timeout: 20 * time.Second}); err != nil {
		return path, err
	}
	return path, nil
}

// Uninstall unloads the job and removes the plist.
func Uninstall(ctx context.Context, run procx.Runner, home string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, _ = run.Run(ctx, procx.Cmd{Argv: []string{"launchctl", "bootout", domain + "/" + Label}, Timeout: 20 * time.Second})
	err := os.Remove(PlistPath(home))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LookPath is exec.LookPath, exposed for tests.
var LookPath = exec.LookPath
