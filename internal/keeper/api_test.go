package keeper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
