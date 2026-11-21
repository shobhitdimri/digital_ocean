package integration

import (
	"testing"
	"time"
)

// TestInvalidationIsScopedToTheFlagThatChanged guards the precision of the
// cache tags: changing one flag must not throw away answers for another.
func TestInvalidationIsScopedToTheFlagThatChanged(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createFlag("flag-a", true)
	s.createFlag("flag-b", true)

	for _, user := range []string{"u1", "u2"} {
		for _, flag := range []string{"flag-a", "flag-b"} {
			s.expectEval(flag, user, true, "global_on", "store")
			s.expectEval(flag, user, true, "global_on", "cache")
		}
	}

	s.updateFlag("flag-a", map[string]any{"enabled": false})

	for _, user := range []string{"u1", "u2"} {
		s.expectEval("flag-a", user, false, "global_off", "store")
		s.expectEval("flag-b", user, true, "global_on", "cache")
	}
}

// TestACachedAnswerExpiresAfterItsTTL uses the real clock and the real janitor.
func TestACachedAnswerExpiresAfterItsTTL(t *testing.T) {
	cfg := testConfig(t)
	cfg.CacheTTL = 250 * time.Millisecond
	s := startServer(t, cfg)
	s.createFlag("short-lived", true)

	s.expectEval("short-lived", "u1", true, "", "store")
	s.expectEval("short-lived", "u1", true, "", "cache")

	waitUntil(t, 5*time.Second, "the cached answer to expire", func() bool {
		return s.evaluate("short-lived", "u1").Source == "store"
	})
	s.expectEval("short-lived", "u1", true, "", "cache")
}

// TestCapacityPressureNeverChangesAnAnswer fills a tiny cache far past its
// capacity. Eviction may cost speed but never correctness.
func TestCapacityPressureNeverChangesAnAnswer(t *testing.T) {
	cfg := testConfig(t)
	cfg.CacheCapacity = 5
	s := startServer(t, cfg)
	s.createFlag("pressured", true)
	s.setOverride("pressured", "odd-user", false)

	for round := 0; round < 3; round++ {
		for i := 0; i < 40; i++ {
			user := "user-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			s.expectEval("pressured", user, true, "global_on", "")
		}
		s.expectEval("pressured", "odd-user", false, "user_override", "")
	}
}
