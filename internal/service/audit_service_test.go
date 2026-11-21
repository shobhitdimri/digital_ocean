package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
)

func TestNewAuditServiceRejectsNil(t *testing.T) {
	if _, err := service.NewAuditService(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
}

func TestAuditHistory(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "r")
	f.svc.Create(ctx, "f", "", true)
	for i := 0; i < 5; i++ {
		f.svc.SetOverride(ctx, "f", fmt.Sprintf("u%d", i), true)
	}
	as, err := service.NewAuditService(f.audit)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("newest first", func(t *testing.T) {
		got, err := as.History(ctx, "f", 3)
		if err != nil || len(got) != 3 {
			t.Fatalf("got %d, %v", len(got), err)
		}
		if got[0].Action != domain.ActionOverrideSet || got[0].ID != "id-6" || got[2].ID != "id-4" {
			t.Fatalf("order wrong: %s %s %s", got[0].ID, got[1].ID, got[2].ID)
		}
	})
	t.Run("zero uses the default", func(t *testing.T) {
		got, err := as.History(ctx, "f", 0)
		if err != nil || len(got) != 6 {
			t.Fatalf("got %d, %v", len(got), err)
		}
	})
	t.Run("huge limit is capped not rejected", func(t *testing.T) {
		if got, err := as.History(ctx, "f", 1<<30); err != nil || len(got) != 6 {
			t.Fatalf("got %d, %v", len(got), err)
		}
	})
	t.Run("negative limit is invalid", func(t *testing.T) {
		if _, err := as.History(ctx, "f", -1); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad name is invalid", func(t *testing.T) {
		if _, err := as.History(ctx, "bad name", 1); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown flag has empty history not 404", func(t *testing.T) {
		got, err := as.History(ctx, "never-existed", 10)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("history survives deletion", func(t *testing.T) {
		f.svc.Delete(ctx, "f")
		got, err := as.History(context.Background(), "f", 1)
		if err != nil || len(got) != 1 || got[0].Action != domain.ActionFlagDeleted {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestDefaultAuditLimitApplies(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)
	for i := 0; i < service.DefaultAuditLimit+5; i++ {
		f.svc.SetOverride(ctx, "f", fmt.Sprintf("u%d", i), true)
	}
	as, _ := service.NewAuditService(f.audit)
	got, err := as.History(ctx, "f", 0)
	if err != nil || len(got) != service.DefaultAuditLimit {
		t.Fatalf("got %d, want %d (%v)", len(got), service.DefaultAuditLimit, err)
	}
}
