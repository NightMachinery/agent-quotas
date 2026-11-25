package agy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CreditRow is one row of `agy -p /credits`: the pay-as-you-go balance
// ("Remaining credits") and the upgrade link. Label is empty for a line
// that is not two tab-separated columns, which is kept as it came.
type CreditRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Credits runs `agy -p /credits` for the named account. It has no cache:
// every call is an agy start-up (seconds), though it spends no quota. An
// account with no stored sign-in is refused before agy starts, so an
// unattended call can never open a browser sign-in.
func (r *Reader) Credits(ctx context.Context, name string) ([]CreditRow, error) {
	a, ok := r.account(name)
	if !ok {
		return nil, fmt.Errorf("no agy account %q in the config", name)
	}
	if r.CacheOnly {
		return nil, errors.New("cache-only mode skips agy/network")
	}
	if StoredAuth(ctx, r.run, a) == AuthMissing {
		return nil, fmt.Errorf("%s: signed out; run agy-acc-login %s", a.Name, a.Name)
	}
	out, err := r.runPrint(ctx, a, "/credits")
	if err != nil {
		return nil, err
	}
	rows := parseCredits(out)
	if len(rows) == 0 {
		return nil, errors.New("no rows in `agy -p /credits'")
	}
	return rows, nil
}

func parseCredits(out string) []CreditRow {
	var rows []CreditRow
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if label, value, ok := strings.Cut(line, "\t"); ok {
			value, _, _ = strings.Cut(value, "\t")
			rows = append(rows, CreditRow{Label: label, Value: value})
			continue
		}
		rows = append(rows, CreditRow{Value: line})
	}
	return rows
}
