// Package memory provides in-memory DAO implementations. They back the unit
// tests and, with a persistence hook, the file store.
package memory

import (
	"context"
	"sort"
	"sync"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
)

var _ dao.FlagDAO = (*FlagDAO)(nil)

// PersistFunc is called, under the store's write lock, with the complete
// post-change state ordered by name. If it returns an error the change is
// rolled back and the error is returned to the caller. It must not retain or
// modify the slice.
type PersistFunc func(flags []domain.Flag) error

// FlagOption configures a FlagDAO.
type FlagOption func(*FlagDAO)

// WithInitial seeds the store. The flags must already be valid.
func WithInitial(flags []domain.Flag) FlagOption {
	return func(d *FlagDAO) {
		for _, f := range flags {
			d.flags[f.Name] = f.Clone()
		}
	}
}

// WithPersist makes every successful mutation write through to fn.
func WithPersist(fn PersistFunc) FlagOption {
	return func(d *FlagDAO) { d.persist = fn }
}

// FlagDAO is a thread-safe, in-memory dao.FlagDAO.
type FlagDAO struct {
	mu      sync.RWMutex
	flags   map[string]domain.Flag
	persist PersistFunc
}

// NewFlagDAO creates an empty store.
func NewFlagDAO(opts ...FlagOption) *FlagDAO {
	d := &FlagDAO{flags: make(map[string]domain.Flag)}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Create implements dao.FlagDAO.
func (d *FlagDAO) Create(ctx context.Context, f domain.Flag) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.flags[f.Name]; exists {
		return domain.AlreadyExistsf("flag %q", f.Name)
	}
	d.flags[f.Name] = f.Clone()
	if err := d.commitLocked(); err != nil {
		delete(d.flags, f.Name)
		return err
	}
	return nil
}

// Get implements dao.FlagDAO.
func (d *FlagDAO) Get(ctx context.Context, name string) (domain.Flag, error) {
	if err := ctx.Err(); err != nil {
		return domain.Flag{}, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	f, ok := d.flags[name]
	if !ok {
		return domain.Flag{}, domain.NotFoundf("flag %q", name)
	}
	return f.Clone(), nil
}

// List implements dao.FlagDAO.
func (d *FlagDAO) List(ctx context.Context) ([]domain.Flag, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	out := make([]domain.Flag, 0, len(d.flags))
	for _, f := range d.flags {
		out = append(out, f.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Update implements dao.FlagDAO.
func (d *FlagDAO) Update(ctx context.Context, f domain.Flag) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	cur, ok := d.flags[f.Name]
	if !ok {
		return domain.NotFoundf("flag %q", f.Name)
	}
	if f.Version != cur.Version+1 {
		return domain.Conflictf("flag %q was modified concurrently (stored version %d, update carries version %d)",
			f.Name, cur.Version, f.Version)
	}
	d.flags[f.Name] = f.Clone()
	if err := d.commitLocked(); err != nil {
		d.flags[f.Name] = cur
		return err
	}
	return nil
}

// Delete implements dao.FlagDAO.
func (d *FlagDAO) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	cur, ok := d.flags[name]
	if !ok {
		return domain.NotFoundf("flag %q", name)
	}
	delete(d.flags, name)
	if err := d.commitLocked(); err != nil {
		d.flags[name] = cur
		return err
	}
	return nil
}

func (d *FlagDAO) commitLocked() error {
	if d.persist == nil {
		return nil
	}
	all := make([]domain.Flag, 0, len(d.flags))
	for _, f := range d.flags {
		all = append(all, f)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return d.persist(all)
}
