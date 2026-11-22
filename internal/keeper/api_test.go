package keeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

type mux struct{ *http.ServeMux }

func (m mux) Handle(p string, h http.HandlerFunc) { m.ServeMux.HandleFunc(p, h) }

func TestHTTPRoutes(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	h.realTurn(sessionA, t0, big)
	m := mux{http.NewServeMux()}
	h.k.Register(m)
	do := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	home := "?home=" + h.home
	if code, out := do("POST", "/v1/agents/"+agentA+"/policy"+home, `{"cache":"warm","set_by":"desk"}`); code != 200 {
		t.Fatalf("policy: %d %v", code, out)
	}
	if code, _ := do("POST", "/v1/agents/"+agentA+"/policy"+home, `{"cache":"warm","set_by":"default"}`); code != 400 {
		t.Fatalf("set_by default accepted from the API: %d", code)
	}
	code, out := do("GET", "/v1/agents/"+agentA+home, "")
	agent, _ := out["agent"].(map[string]any)
	cache, _ := agent["cache"].(map[string]any)
	if code != 200 || cache["phase"] != PhaseWarming || cache["set_by"] != "desk" || agent["lock_path"] == nil {
		t.Fatalf("get: %d %v", code, out)
	}
	if code, out := do("GET", "/v1/agents?policy=warm", ""); code != 200 || len(out["agents"].([]any)) != 1 || out["keeper_enabled"] != true {
		t.Fatalf("list: %d %v", code, out)
	}
	acCase := func(body string, want int, enabled bool, source string) {
		t.Helper()
		code, out := do("POST", "/v1/agents/"+agentA+"/auto-continue"+home, body)
		if code != want {
			t.Fatalf("auto-continue %s: %d %v", body, code, out)
		}
		if want != 200 {
			return
		}
		agent, _ := out["agent"].(map[string]any)
		ac, _ := agent["auto_continue"].(map[string]any)
		if ac["enabled"] != enabled || ac["set_by"] != source {
			t.Fatalf("auto-continue %s: %v", body, ac)
		}
	}
	acCase(`{"enabled":true,"set_by":"default"}`, 200, true, "default")
	acCase(`{"enabled":false,"set_by":"desk"}`, 200, false, "desk")
	acCase(`{"enabled":true,"set_by":"default"}`, 200, false, "desk")
	acCase(`{"enabled":true,"set_by":"default","reset":true}`, 200, true, "default")
	acCase(`{"enabled":true,"reset":true}`, 400, false, "")
	acCase(`{"enabled":true,"set_by":"bogus"}`, 400, false, "")
	acCase(`{"enabled":false}`, 200, false, "api")
	if code, _ := do("GET", "/v1/agents/"+agentB+home, ""); code != 404 {
		t.Fatalf("unknown agent: %d", code)
	}
	code, out = do("POST", "/v1/agents/"+agentA+"/compact"+home, "")
	if code != 200 || out["compaction_id"] == nil {
		t.Fatalf("compact: %d %v", code, out)
	}
	code, out = do("GET", "/v1/agents/"+agentA+home+"&wait_compaction="+out["compaction_id"].(string)+"&timeout=0.05", "")
	if code != 200 {
		t.Fatalf("long-poll: %d %v", code, out)
	}
	off := false
	h.cfg.Keeper.Enabled = &off
	if code, out := do("POST", "/v1/agents/"+agentA+"/standby"+home, ""); code != 503 || out["code"] != "keeper_disabled" {
		t.Fatalf("disabled standby: %d %v", code, out)
	}
	if code, _ := do("GET", "/v1/agents", ""); code != 200 {
		t.Fatalf("disabled list: %d", code)
	}
}

func TestAutoContinueHTTPRoutes(t *testing.T) {
	h := newHarness(t)
	h.agent(agentA, sessionA)
	m := mux{http.NewServeMux()}
	h.k.Register(m)
	path := "/v1/agents/" + agentA + "/auto-continue?home=" + h.home
	do := func(method, path, body string, status int) Document {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		if status != http.StatusOK {
			return Document{}
		}
		var doc Document
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	for _, body := range []string{"{}", `{"enabled":null}`, `{"enabled":"false"}`} {
		do("POST", path, body, http.StatusBadRequest)
	}
	do("POST", strings.Replace(path, agentA, agentB, 1), `{"enabled":true}`, http.StatusNotFound)
	off := false
	h.cfg.Keeper.Enabled = &off
	doc := do("POST", path, `{"enabled":false}`, http.StatusOK)
	if ac := doc.Agent.AutoContinue; doc.SchemaVersion == 0 || doc.Agent.KeeperEnabled ||
		!doc.Agent.AutoContinueEngineEnabled || doc.Agent.ID != agentA || ac == nil || ac.Enabled || ac.SetBy != "api" {
		t.Fatalf("explicit off without a prior registration: %+v, auto-continue %+v", doc, ac)
	}

	on := true
	h.cfg.Keeper.Enabled = &on
	h.set(agentA, PolicyWarm)
	if _, err := h.eng.On(context.Background(), agentA, h.home, 120); err != nil {
		t.Fatal(err)
	}
	state, err := paseo.ReadObject(h.path(agentA))
	if err != nil {
		t.Fatal(err)
	}
	state.Set("claimed_failure", "synthetic claim")
	if err := paseo.AtomicWrite(h.path(agentA), state); err != nil {
		t.Fatal(err)
	}
	cache, _ := state.Object("cache")
	var queued []string
	h.k.AutoContinueChanged = func(path string) { queued = append(queued, path) }

	doc = do("POST", path, `{"enabled":true}`, http.StatusOK)
	if ac := doc.Agent.AutoContinue; ac == nil || !ac.Enabled || ac.SetBy != "api" {
		t.Fatalf("repeated on: %+v", ac)
	}
	state, err = paseo.ReadObject(h.path(agentA))
	if err != nil {
		t.Fatal(err)
	}
	if claim, _ := state.String("claimed_failure"); claim != "synthetic claim" || state.Value("poll") != json.Number("120") {
		t.Fatalf("repeated on lost claim or poll: %s, %v", claim, state.Value("poll"))
	}
	if len(queued) != 1 || queued[0] != h.path(agentA) {
		t.Fatalf("queued %v", queued)
	}
	do("POST", path, `{"enabled":false}`, http.StatusOK)
	doc = do("GET", "/v1/agents/"+agentA+"?home="+h.home, "", http.StatusOK)
	if ac := doc.Agent.AutoContinue; ac == nil || ac.Enabled || ac.Status != paseo.StatusOff || ac.SetBy != "api" {
		t.Fatalf("GET after off: %+v", ac)
	}
	state, err = paseo.ReadObject(h.path(agentA))
	if err != nil {
		t.Fatal(err)
	}
	gotCache, _ := state.Object("cache")
	before, _ := paseo.Dumps(cache, false)
	after, _ := paseo.Dumps(gotCache, false)
	if before != after || len(queued) != 1 {
		t.Fatalf("off changed the cache or enqueued a check: %s -> %s, %v", before, after, queued)
	}
}
