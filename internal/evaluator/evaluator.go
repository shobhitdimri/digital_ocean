// Package evaluator decides whether a flag is enabled for a user.
//
// Design (SOLID):
//
//   - Single responsibility: the Evaluator only runs an ordered chain of Rules.
//     Each Rule decides one concern. Caching, auditing and logging live in the
//     service layer, so the evaluator stays pure and easy to test.
//   - Open/closed: new behaviour (percentage rollout, prerequisites, ...) is a
//     new Rule added to the chain. The Evaluator and the existing rules do not
//     change.
//   - Liskov: every Rule honours the same contract (see Rule), so any rule can
//     be placed anywhere in the chain.
//   - Interface segregation: rules that need other flags receive only
//     FlagLookup, one read-only method, not a whole store.
//   - Dependency inversion: FlagLookup is declared here, by the consumer. The
//     evaluator imports nothing but the domain; any store with a matching Get
//     method (such as dao.FlagReader) satisfies it.
package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"feature-flag-service/internal/domain"
)

// ErrNoDecision means the rule chain finished without any rule deciding. It
// indicates a misconfigured chain (no terminal rule), not bad input, so it does
// not wrap a domain sentinel and surfaces as an internal error.
var ErrNoDecision = errors.New("evaluator: no rule produced a decision")

// ErrDependencyCycle means evaluating a flag led back to a flag already being
// evaluated, or went deeper than MaxDependencyDepth. The service refuses to
// store such a graph, so this only happens when the data was edited by hand. It
// does not wrap a domain sentinel: it is a data fault, not bad input, and
// surfaces as an internal error.
var ErrDependencyCycle = errors.New("evaluator: flag dependencies form a cycle or nest too deeply")

// MaxDependencyDepth bounds how many flags deep a prerequisite chain may go.
const MaxDependencyDepth = 32

// Reason explains why an evaluation produced its result.
type Reason string

const (
	ReasonUserOverride Reason = "user_override"
	ReasonGlobalOn     Reason = "global_on"
	ReasonGlobalOff    Reason = "global_off"

	// ReasonRolloutIncluded and ReasonRolloutExcluded are reported for an
	// enabled flag whose rollout is below 100%: the user's bucket fell inside or
	// outside the rolled-out share.
	ReasonRolloutIncluded Reason = "rollout_included"
	ReasonRolloutExcluded Reason = "rollout_excluded"

	// ReasonPrerequisiteUnmet is reported when a required flag is off for the
	// user, or no longer exists.
	ReasonPrerequisiteUnmet Reason = "prerequisite_unmet"
)

// FlagLookup reads flags by name. It is the only capability the evaluator and
// its rules need from storage.
type FlagLookup interface {
	// Get returns the flag, or an error wrapping domain.ErrNotFound.
	Get(ctx context.Context, name string) (domain.Flag, error)
}

// Input is what a Rule sees. Rules must treat it as read-only.
type Input struct {
	Flag   domain.Flag
	UserID string

	// Flags looks up other flags, for rules that depend on them. Every name
	// looked up is recorded in Result.Consulted.
	Flags FlagLookup

	// Evaluate evaluates another flag for the same user through the whole rule
	// chain, so a prerequisite's own overrides, rollout and prerequisites all
	// count. The flag is recorded in Result.Consulted. It returns an error
	// wrapping domain.ErrNotFound when the flag does not exist and
	// ErrDependencyCycle when the flags loop. Answers are remembered for the
	// rest of the evaluation, so a flag required twice is evaluated once.
	Evaluate func(ctx context.Context, flagName string) (Decision, error)
}

// Decision is a rule's verdict.
type Decision struct {
	Enabled bool
	Reason  Reason
}

// Rule decides one concern of an evaluation.
//
// Contract, which every implementation must honour:
//
//   - Apply returns decided=false to pass to the next rule, and decided=true
//     with a Decision to end the evaluation.
//   - Apply must not modify in.Flag (including its Overrides map) and must not
//     keep a reference to Input after returning.
//   - Apply must be safe for concurrent use, so rules should be stateless.
//   - Errors must wrap domain sentinels where one applies; they stop the
//     evaluation and are returned to the caller.
type Rule interface {
	// Name identifies the rule in results and logs.
	Name() string
	Apply(ctx context.Context, in Input) (d Decision, decided bool, err error)
}

// Result is the outcome of an evaluation. It is a plain value, safe to cache.
type Result struct {
	Flag    string
	UserID  string
	Enabled bool
	Reason  Reason

	// DecidedBy is the name of the rule that produced the decision.
	DecidedBy string

	// Consulted lists every flag read to reach the result, starting with the
	// evaluated flag itself. The result stays valid only while all of them are
	// unchanged, so callers use it as the cache invalidation tags.
	Consulted []string
}

// Evaluator runs a rule chain. It holds no mutable state, so one instance is
// safe for concurrent use.
type Evaluator struct {
	lookup FlagLookup
	rules  []Rule
}

// New builds an Evaluator that runs rules in order; the first rule to decide
// wins. The chain should end with a terminal rule that always decides.
func New(lookup FlagLookup, rules ...Rule) (*Evaluator, error) {
	if lookup == nil {
		return nil, errors.New("evaluator: lookup must not be nil")
	}
	if len(rules) == 0 {
		return nil, errors.New("evaluator: at least one rule is required")
	}
	for i, r := range rules {
		if r == nil {
			return nil, fmt.Errorf("evaluator: rule %d is nil", i)
		}
	}
	return &Evaluator{lookup: lookup, rules: append([]Rule(nil), rules...)}, nil
}

// NewDefault builds an Evaluator with the standard rule chain.
func NewDefault(lookup FlagLookup) (*Evaluator, error) {
	return New(lookup, DefaultRules()...)
}

// Evaluate loads the named flag and evaluates it for the user. It returns an
// error wrapping domain.ErrInvalid for a malformed flag name or user id, and
// one wrapping domain.ErrNotFound when the flag does not exist.
func (e *Evaluator) Evaluate(ctx context.Context, flagName, userID string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := domain.ValidateName(flagName); err != nil {
		return Result{}, err
	}
	if err := domain.ValidateUserID(userID); err != nil {
		return Result{}, err
	}

	rec := newRecorder(e.lookup)
	flag, err := rec.Get(ctx, flagName)
	if err != nil {
		return Result{}, err
	}
	return e.run(ctx, flag, userID, rec)
}

// EvaluateFlag evaluates an already loaded flag. It is the pure core of
// Evaluate and needs no storage unless a rule looks up other flags.
func (e *Evaluator) EvaluateFlag(ctx context.Context, flag domain.Flag, userID string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := domain.ValidateUserID(userID); err != nil {
		return Result{}, err
	}
	rec := newRecorder(e.lookup)
	rec.note(flag.Name)
	return e.run(ctx, flag, userID, rec)
}

func (e *Evaluator) run(ctx context.Context, flag domain.Flag, userID string, rec *recorder) (Result, error) {
	sess := &session{e: e, rec: rec, userID: userID, memo: make(map[string]memoEntry)}
	d, decidedBy, err := sess.decide(ctx, flag)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Flag:      flag.Name,
		UserID:    userID,
		Enabled:   d.Enabled,
		Reason:    d.Reason,
		DecidedBy: decidedBy,
		Consulted: rec.names(),
	}, nil
}

type memoEntry struct {
	d   Decision
	err error
}

// session is the state of one evaluation: whose answer is wanted, which flags
// have been read, which are being evaluated right now (to catch loops) and
// which are already answered. It lives for one call and is used by one
// goroutine, so it needs no locking.
type session struct {
	e      *Evaluator
	rec    *recorder
	userID string
	memo   map[string]memoEntry
	stack  []string
}

// decide runs the rule chain for one flag and returns the verdict with the name
// of the rule that gave it.
func (s *session) decide(ctx context.Context, flag domain.Flag) (Decision, string, error) {
	if len(s.stack) >= MaxDependencyDepth {
		return Decision{}, "", fmt.Errorf("%w: more than %d flags deep at %q", ErrDependencyCycle, MaxDependencyDepth, flag.Name)
	}
	s.stack = append(s.stack, flag.Name)
	defer func() { s.stack = s.stack[:len(s.stack)-1] }()

	in := Input{Flag: flag, UserID: s.userID, Flags: s.rec, Evaluate: s.resolve}
	for _, rule := range s.e.rules {
		d, decided, err := rule.Apply(ctx, in)
		if err != nil {
			return Decision{}, "", fmt.Errorf("rule %q: %w", rule.Name(), err)
		}
		if decided {
			return d, rule.Name(), nil
		}
	}
	return Decision{}, "", fmt.Errorf("%w for flag %q", ErrNoDecision, flag.Name)
}

// resolve answers a rule's request to evaluate another flag.
func (s *session) resolve(ctx context.Context, name string) (Decision, error) {
	for _, active := range s.stack {
		if active == name {
			return Decision{}, fmt.Errorf("%w: %s -> %s", ErrDependencyCycle, strings.Join(s.stack, " -> "), name)
		}
	}
	if m, ok := s.memo[name]; ok {
		return m.d, m.err
	}
	flag, err := s.rec.Get(ctx, name)
	if err == nil {
		var d Decision
		d, _, err = s.decide(ctx, flag)
		if err == nil {
			s.memo[name] = memoEntry{d: d}
			return d, nil
		}
	}
	// A cancelled context is not a property of the flag, so it is not remembered.
	if ctx.Err() == nil {
		s.memo[name] = memoEntry{err: err}
	}
	return Decision{}, err
}

// recorder decorates a FlagLookup to remember which flags were read. It is
// created per evaluation, so it needs no locking and never leaks state between
// concurrent evaluations. Rules stay unaware of it.
type recorder struct {
	inner FlagLookup
	order []string
	seen  map[string]struct{}
}

func newRecorder(inner FlagLookup) *recorder {
	return &recorder{inner: inner, seen: make(map[string]struct{})}
}

// Get records the name before delegating, even if the lookup then fails: a
// result that depended on a flag being absent changes when that flag is
// created, so the missing flag must still be tracked.
func (r *recorder) Get(ctx context.Context, name string) (domain.Flag, error) {
	r.note(name)
	return r.inner.Get(ctx, name)
}

func (r *recorder) note(name string) {
	if _, ok := r.seen[name]; ok {
		return
	}
	r.seen[name] = struct{}{}
	r.order = append(r.order, name)
}

// names returns the recorded names. The recorder is discarded after one
// evaluation, so the slice is never shared between results.
func (r *recorder) names() []string {
	return r.order
}
