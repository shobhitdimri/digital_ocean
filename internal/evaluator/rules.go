package evaluator

import "context"

// Compile-time proof that each rule satisfies the Rule contract.
var (
	_ Rule = OverrideRule{}
	_ Rule = GlobalOffRule{}
	_ Rule = GlobalOnRule{}
)

// DefaultRules is the standard chain. The order is the evaluation rule set:
//
//  1. a per-user override wins over everything below it
//  2. otherwise a globally disabled flag is off
//  3. otherwise the flag is on
//
// Extensions slot in here without touching the Evaluator: percentage rollout
// goes between GlobalOffRule and GlobalOnRule, and prerequisites go first.
func DefaultRules() []Rule {
	return []Rule{
		OverrideRule{},
		GlobalOffRule{},
		GlobalOnRule{},
	}
}

// OverrideRule applies a per-user override, if the user has one.
type OverrideRule struct{}

// Name implements Rule.
func (OverrideRule) Name() string { return "override" }

// Apply implements Rule.
func (OverrideRule) Apply(_ context.Context, in Input) (Decision, bool, error) {
	if enabled, ok := in.Flag.OverrideFor(in.UserID); ok {
		return Decision{Enabled: enabled, Reason: ReasonUserOverride}, true, nil
	}
	return Decision{}, false, nil
}

// GlobalOffRule turns the flag off for everyone when it is globally disabled.
// It passes when the flag is enabled, leaving later rules to refine the answer.
type GlobalOffRule struct{}

// Name implements Rule.
func (GlobalOffRule) Name() string { return "global_off" }

// Apply implements Rule.
func (GlobalOffRule) Apply(_ context.Context, in Input) (Decision, bool, error) {
	if !in.Flag.Enabled {
		return Decision{Enabled: false, Reason: ReasonGlobalOff}, true, nil
	}
	return Decision{}, false, nil
}

// GlobalOnRule is the terminal rule: a flag that reached it is on.
type GlobalOnRule struct{}

// Name implements Rule.
func (GlobalOnRule) Name() string { return "global_on" }

// Apply implements Rule.
func (GlobalOnRule) Apply(context.Context, Input) (Decision, bool, error) {
	return Decision{Enabled: true, Reason: ReasonGlobalOn}, true, nil
}
