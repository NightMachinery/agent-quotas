package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/NightMachinery/agent-quotas/internal/agy"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

type fakeCredits map[string][]agy.CreditRow

func (f fakeCredits) Credits(_ context.Context, name string) ([]agy.CreditRow, error) {
	if rows, ok := f[name]; ok {
		return rows, nil
	}
	return nil, errors.New("`agy -p /credits' failed (3)")
}

func TestPrintCredits(t *testing.T) {
	snaps := []model.Snapshot{
		{Name: "m", Verdict: model.Usable},
		{Name: "cave", Verdict: model.SignedOut},
		{Name: "fm2", Verdict: model.Blocked},
	}
	var out bytes.Buffer
	code := printCredits(context.Background(), &out, fakeCredits{
		"m": {{Label: "Remaining credits", Value: "0"}, {Label: "Upgrade", Value: "https://example.test"}},
	}, snaps)
	want := "Credits (agy -p /credits)\n" +
		"  m    Remaining credits: 0\n" +
		"  m    Upgrade: https://example.test\n" +
		"  fm2  credits failed: `agy -p /credits' failed (3)\n"
	if out.String() != want || code != exitFail {
		t.Errorf("code %d, output:\n%s", code, out.String())
	}
}

func TestAgyStatusFlagConflicts(t *testing.T) {
	for _, args := range [][]string{
		{"--flat", "--json"},
		{"--credits", "--json"},
		{"--credits", "--flat"},
		{"--max-age", "60"},
		{"--flat", "m", "cave"},
	} {
		if code := cmdAgyStatus(context.Background(), args); code != exitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, exitUsage)
		}
	}
}
