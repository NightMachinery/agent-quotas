package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverAgyAccountsSkipsNonAccountDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{"alpha/antigravity-cli", "beta/antigravity-cli", "history-owners"} {
		if err := os.MkdirAll(filepath.Join(home, ".agy-accounts", dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := Discover(func(string) string { return "" })
	var names []string
	for _, a := range c.Agy.Accounts {
		names = append(names, a.Name)
	}
	if want := []string{"m", "alpha", "beta"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("agy accounts = %v, want %v", names, want)
	}
}
