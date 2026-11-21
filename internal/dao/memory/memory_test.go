package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/dao/daotest"
	"feature-flag-service/internal/domain"
)

func TestFlagDAOContract(t *testing.T) {
	daotest.RunFlagDAOContract(t, func(t *testing.T) dao.FlagDAO { return NewFlagDAO() })
}

func TestAuditDAOContract(t *testing.T) {
	daotest.RunAuditDAOContract(t, func(t *testing.T) dao.AuditDAO { return NewAuditDAO() })
}

func TestWithInitialSeedsIndependentCopies(t *testing.T) {
	seed, err := domain.NewFlag("seeded", "d", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = seed.SetOverride("alice", true)

	d := NewFlagDAO(WithInitial([]domain.Flag{seed}))
	seed.Overrides["alice"] = false

	got, err := d.Get(context.Background(), "seeded")
	if err != nil || !got.Overrides["alice"] {
		t.Fatalf("seeded flag = %+v, %v; want override preserved", got, err)
	}
}

func TestPersistFailureRollsBackEveryMutation(t *testing.T) {
	ctx := context.Background()
	fail := false
	persistErr := errors.New("disk full")
	d := NewFlagDAO(WithPersist(func([]domain.Flag) error {
		if fail {
			return persistErr
		}
		return nil
	}))

	existing, _ := domain.NewFlag("existing", "v1", false, time.Now())
	if err := d.Create(ctx, existing); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fail = true

	fresh, _ := domain.NewFlag("fresh", "", false, time.Now())
	if err := d.Create(ctx, fresh); !errors.Is(err, persistErr) {
		t.Fatalf("Create: want persist error, got %v", err)
	}
	if _, err := d.Get(ctx, "fresh"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed Create must be rolled back, Get = %v", err)
	}

	next := existing.Clone()
	next.Version = 2
	next.Description = "v2"
	if err := d.Update(ctx, next); !errors.Is(err, persistErr) {
		t.Fatalf("Update: want persist error, got %v", err)
	}
	if got, _ := d.Get(ctx, "existing"); got.Description != "v1" || got.Version != 1 {
		t.Fatalf("failed Update must be rolled back, got %+v", got)
	}

	if err := d.Delete(ctx, "existing"); !errors.Is(err, persistErr) {
		t.Fatalf("Delete: want persist error, got %v", err)
	}
	if _, err := d.Get(ctx, "existing"); err != nil {
		t.Fatalf("failed Delete must be rolled back, Get = %v", err)
	}
}

func TestPersistReceivesSortedFullState(t *testing.T) {
	var seen []string
	d := NewFlagDAO(WithPersist(func(flags []domain.Flag) error {
		seen = seen[:0]
		for _, f := range flags {
			seen = append(seen, f.Name)
		}
		return nil
	}))
	for _, n := range []string{"b", "c", "a"} {
		f, _ := domain.NewFlag(n, "", false, time.Now())
		if err := d.Create(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 3 || seen[0] != "a" || seen[1] != "b" || seen[2] != "c" {
		t.Fatalf("persist saw %v, want [a b c]", seen)
	}
}
