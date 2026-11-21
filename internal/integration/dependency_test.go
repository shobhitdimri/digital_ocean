package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

func (s *server) createDependent(name string, enabled bool, requires ...string) flagBody {
	s.t.Helper()
	r := s.mustStatus(s.call("POST", "/v1/flags", "tester",
		map[string]any{"name": name, "enabled": enabled, "requires": requires}), 201, "create "+name)
	return decode[flagBody](s.t, r)
}

func (s *server) setRequires(name string, requires ...string) response {
	s.t.Helper()
	if requires == nil {
		requires = []string{}
	}
	return s.call("PUT", "/v1/flags/"+name, "tester", map[string]any{"requires": requires})
}

func TestPrerequisiteGatesTheDependentAndTracksItLive(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createFlag("payments", true)
	created := s.createDependent("checkout", true, "payments")
	if len(created.Requires) != 1 || created.Requires[0] != "payments" {
		t.Fatalf("created = %+v", created)
	}

	s.expectEval("checkout", "u1", true, "global_on", "store")
	s.expectEval("checkout", "u1", true, "global_on", "cache")

	// Turning the prerequisite off must reach the dependent's cached answer.
	s.updateFlag("payments", map[string]any{"enabled": false})
	s.expectEval("checkout", "u1", false, "prerequisite_unmet", "store")
	s.expectEval("checkout", "u1", false, "prerequisite_unmet", "cache")

	s.updateFlag("payments", map[string]any{"enabled": true})
	s.expectEval("checkout", "u1", true, "global_on", "store")
}

func TestOverrideCannotOpenAGatedFlagAndRolloutOfThePrerequisiteLimitsIt(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createRolloutFlag("payments", true, 0)
	s.createDependent("checkout", true, "payments")
	s.setOverride("checkout", "vip", true)

	s.expectEval("checkout", "vip", false, "prerequisite_unmet", "store")
	s.expectEval("checkout", "anyone", false, "prerequisite_unmet", "store")

	s.setRollout("payments", 100)
	s.expectEval("checkout", "vip", true, "user_override", "store")
	s.expectEval("checkout", "anyone", true, "global_on", "store")
}

func TestChainsAreFollowedAndOnlyTheChangedFlagsDependentsAreRecomputed(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createFlag("root", true)
	s.createDependent("mid", true, "root")
	s.createDependent("leaf", true, "mid")
	s.createFlag("unrelated", true)

	s.expectEval("leaf", "u", true, "", "store")
	s.expectEval("unrelated", "u", true, "", "store")

	s.updateFlag("root", map[string]any{"enabled": false})
	s.expectEval("leaf", "u", false, "prerequisite_unmet", "store")
	s.expectEval("unrelated", "u", true, "global_on", "cache")
}

func TestCyclesAndDanglingReferencesAreRejected(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createFlag("a", true)
	s.createDependent("b", true, "a")
	s.createDependent("c", true, "b")

	check := func(what string, r response, status int, code string) {
		t.Helper()
		if r.Status != status || decode[errorBody](t, r).Error.Code != code {
			t.Fatalf("%s: %d %s, want %d %s", what, r.Status, r.Body, status, code)
		}
	}
	r := s.setRequires("a", "c")
	check("indirect cycle", r, 409, "conflict")
	if !strings.Contains(decode[errorBody](t, r).Error.Message, "a -> c -> b -> a") {
		t.Fatalf("the message should show the loop: %s", r.Body)
	}
	check("self reference", s.setRequires("a", "a"), 400, "invalid_request")
	check("unknown flag on update", s.setRequires("a", "ghost"), 400, "invalid_request")
	check("unknown flag on create", s.call("POST", "/v1/flags", "tester", map[string]any{"name": "d", "requires": []string{"ghost"}}), 400, "invalid_request")
	if r := s.call("GET", "/v1/flags/d", "", nil); r.Status != 404 {
		t.Fatalf("a rejected create left a flag behind: %d", r.Status)
	}
	got := decode[flagBody](t, s.mustStatus(s.call("GET", "/v1/flags/a", "", nil), 200, "get a"))
	if got.Version != 1 || len(got.Requires) != 0 {
		t.Fatalf("rejected updates changed the flag: %+v", got)
	}
}

func TestARequiredFlagCannotBeDeletedUntilReleased(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createFlag("payments", true)
	s.createDependent("checkout", true, "payments")

	r := s.call("DELETE", "/v1/flags/payments", "ops", nil)
	if r.Status != 409 || !strings.Contains(decode[errorBody](t, r).Error.Message, "checkout") {
		t.Fatalf("got %d %s, want 409 naming checkout", r.Status, r.Body)
	}
	s.expectEval("checkout", "u", true, "global_on", "")

	s.mustStatus(s.setRequires("checkout"), 200, "clear requires")
	s.mustStatus(s.call("DELETE", "/v1/flags/payments", "ops", nil), 204, "delete after release")
	s.expectEval("checkout", "u", true, "global_on", "store")
}

func TestPrerequisitesAreAuditedAndSurviveARestart(t *testing.T) {
	cfg := testConfig(t)
	s := startServer(t, cfg)
	s.createFlag("payments", true)
	s.createFlag("checkout", true)
	s.mustStatus(s.call("PUT", "/v1/flags/checkout", "carol", map[string]any{"requires": []string{"payments"}}), 200, "set")

	h := s.audit("checkout")
	if len(h) != 2 || h[0].Actor != "carol" || len(h[0].Before.Requires) != 0 || len(h[0].After.Requires) != 1 {
		t.Fatalf("history = %+v", h)
	}
	s.stop()

	onDisk := readFile(t, filepath.Join(cfg.DataDir, "flags.json"))
	if !strings.Contains(onDisk, `"requires"`) || !strings.Contains(onDisk, `"schema_version": 3`) {
		t.Fatalf("flags.json:\n%s", onDisk)
	}
	s2 := startServer(t, cfg)
	got := decode[flagBody](t, s2.mustStatus(s2.call("GET", "/v1/flags/checkout", "", nil), 200, "get"))
	if len(got.Requires) != 1 || got.Requires[0] != "payments" {
		t.Fatalf("after restart: %+v", got)
	}
	s2.updateFlag("payments", map[string]any{"enabled": false})
	s2.expectEval("checkout", "u", false, "prerequisite_unmet", "store")
}
