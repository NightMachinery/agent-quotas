package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// The golden files come from the old Python tool itself; see
// testdata/gen_golden.py.

type goldenCase struct {
	Name     string       `json:"name"`
	Label    string       `json:"label"`
	Width    int          `json:"width"`
	Keychain keychainInfo `json:"keychain"`
	Token    *struct {
		Source           string   `json:"source"`
		ExpiresAtS       *float64 `json:"expires_at_s"`
		SubscriptionType *string  `json:"subscription_type"`
	} `json:"token"`
	Warnings    []string `json:"warnings"`
	Error       string   `json:"error"`
	ViaSnapshot *struct {
		FromDaemonCache bool    `json:"from_daemon_cache"`
		StaleReason     *string `json:"stale_reason"`
	} `json:"via_snapshot"`
	Result *struct {
		Source      string          `json:"source"`
		FromCache   bool            `json:"from_cache"`
		StaleReason *string         `json:"stale_reason"`
		FetchedAtS  float64         `json:"fetched_at_s"`
		Payload     json.RawMessage `json:"payload"`
	} `json:"result"`
}

type goldenFile struct {
	Now   float64      `json:"now"`
	TZ    string       `json:"tz"`
	Cases []goldenCase `json:"cases"`
}

func loadGolden(t *testing.T) (goldenFile, *time.Location) {
	t.Helper()
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(g.TZ)
	if err != nil {
		t.Fatal(err)
	}
	return g, loc
}

// rowFor builds the Go legacy row for a case, through LegacyRow for the
// cases that model a daemon-cached reading.
func rowFor(t *testing.T, c goldenCase, now float64, loc *time.Location) json.RawMessage {
	t.Helper()
	if c.Error != "" {
		return mustJSON(errorRow{Profile: strPtrOrNil(c.Label), Error: c.Error, Keychain: c.Keychain})
	}
	payload, err := parseValue(c.Result.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var tok *tokenInfo
	if c.Token != nil {
		tok = &tokenInfo{token: "synthetic", source: c.Token.Source, expiresAt: c.Token.ExpiresAtS, subscription: c.Token.SubscriptionType}
	}
	res := &fetchResult{payload: payload, fetchedAt: c.Result.FetchedAtS, fromCache: c.Result.FromCache,
		staleReason: c.Result.StaleReason, source: c.Result.Source}
	if c.ViaSnapshot == nil {
		return mustJSON(buildRow(c.Label, tok, res, c.Warnings, c.Keychain, now, loc))
	}
	// As the daemon would have it: a fresh api reading, served later.
	res.source, res.fromCache, res.staleReason = "api", false, nil
	s := model.Snapshot{Name: c.Label, Native: mustJSON(buildRow(c.Label, tok, res, c.Warnings, c.Keychain, now, loc)),
		FetchedAt: model.F(c.Result.FetchedAtS)}
	if c.ViaSnapshot.StaleReason != nil {
		s.Stale, s.StaleReason = true, c.ViaSnapshot.StaleReason
	}
	return LegacyRow(s, c.ViaSnapshot.FromDaemonCache)
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func diffLines(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			fmt.Fprintf(&b, "  line %d:\n    want %q\n    got  %q\n", i+1, wl, gl)
		}
	}
	return b.String()
}

func TestGoldenHuman(t *testing.T) {
	g, loc := loadGolden(t)
	now := time.Unix(int64(g.Now), 0).In(loc)
	var all []json.RawMessage
	for _, c := range g.Cases {
		row := rowFor(t, c, g.Now, loc)
		all = append(all, row)
		for ext, opts := range map[string]RenderOptions{
			"txt":       {Width: c.Width, Now: now},
			"color.txt": {Color: true, Width: c.Width, Now: now},
			"neon.txt":  {Color: true, TrueColor: true, Width: c.Width, Now: now},
		} {
			var buf bytes.Buffer
			if err := RenderHuman(&buf, []json.RawMessage{row}, opts); err != nil {
				t.Fatal(err)
			}
			if want := readGolden(t, c.Name+"."+ext); buf.String() != want {
				t.Errorf("%s.%s differs:\n%s", c.Name, ext, diffLines(want, buf.String()))
			}
		}
		var buf bytes.Buffer
		if err := RenderCloudCredit(&buf, []json.RawMessage{row}, RenderOptions{Width: c.Width, Now: now}); err != nil {
			t.Fatal(err)
		}
		want := readGolden(t, c.Name+".cloud.txt")
		if want == "\n" {
			want = "No cloud credit.\n"
		}
		if buf.String() != want {
			t.Errorf("%s.cloud.txt differs:\n%s", c.Name, diffLines(want, buf.String()))
		}
	}
	var buf bytes.Buffer
	if err := RenderHuman(&buf, all, RenderOptions{Width: 100, Now: now}); err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "all.txt"); buf.String() != want {
		t.Errorf("all.txt differs:\n%s", diffLines(want, buf.String()))
	}
	buf.Reset()
	if err := RenderCloudCredit(&buf, all, RenderOptions{Width: 100, Now: now}); err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "all.cloud.txt"); buf.String() != want {
		t.Errorf("all.cloud.txt differs:\n%s", diffLines(want, buf.String()))
	}
}

func TestGoldenJSON(t *testing.T) {
	g, loc := loadGolden(t)
	for _, c := range g.Cases {
		var got, want map[string]any
		if err := json.Unmarshal(rowFor(t, c, g.Now, loc), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(readGolden(t, c.Name+".json")), &want); err != nil {
			t.Fatal(err)
		}
		delete(got, "token_expires_at") // the one addition to the old row
		if !reflect.DeepEqual(got, want) {
			gj, _ := json.MarshalIndent(got, "", " ")
			wj, _ := json.MarshalIndent(want, "", " ")
			t.Errorf("%s.json differs:\n%s", c.Name, diffLines(string(wj), string(gj)))
		}
	}
}
