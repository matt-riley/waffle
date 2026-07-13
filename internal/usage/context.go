package usage

import "context"

type budgetKeyContextKey struct{}

// WithBudgetKey groups accounting across multiple concrete sessions, such as
// fresh transcript sessions created for retries of one unattended job.
func WithBudgetKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, budgetKeyContextKey{}, key)
}

// BudgetKey returns an explicit accounting group or the concrete session.
func BudgetKey(ctx context.Context, session string) string {
	if key, ok := ctx.Value(budgetKeyContextKey{}).(string); ok && key != "" {
		return key
	}
	return session
}
