package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NightMachinery/agent-quotas/internal/procx"
)

func TestRenderPlist(t *testing.T) {
	text, err := Render(Spec{Label: Label, Binary: "/opt/x/agent-quotas", LogPath: "/tmp/s/daemon.log",
		Path: "/opt/x:/usr/bin", Home: "/home/u&v"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>serve</string>", "<key>KeepAlive</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>", "/home/u&amp;v", "<string>/opt/x:/usr/bin</string>", Label} {
		if !strings.Contains(text, want) {
			t.Errorf("plist lacks %q:\n%s", want, text)
		}
	}
}

func TestSearchPath(t *testing.T) {
	look := func(name string) (string, error) {
		switch name {
		case "codex":
			return "/opt/homebrew/bin/codex", nil
		case "paseo":
			return "/home/u/.local/bin/paseo", nil
		}
		return "", errors.New("absent")
	}
	got := SearchPath("/home/u/go/bin/agent-quotas", look, "/home/u")
	want := "/home/u/go/bin:/home/u/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	if got != want {
		t.Fatalf("PATH\n got %s\nwant %s", got, want)
	}
}

type fakeRun struct{ argvs [][]string }

func (f *fakeRun) Run(_ context.Context, c procx.Cmd) (procx.Result, error) {
	f.argvs = append(f.argvs, c.Argv)
	return procx.Result{}, nil
}

func TestInstallUninstallWithFakeLaunchctl(t *testing.T) {
	home := t.TempDir()
	run := &fakeRun{}
	path, err := Install(context.Background(), run, Spec{Label: Label, Binary: "/b", LogPath: "/l", Path: "/p", Home: home}, home)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "Library", "LaunchAgents", Label+".plist") {
		t.Fatalf("path %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if len(run.argvs) != 2 || run.argvs[1][1] != "bootstrap" {
		t.Fatalf("launchctl calls %v", run.argvs)
	}
	if err := Uninstall(context.Background(), run, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
}
