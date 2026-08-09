package dashboard

import (
	"github.com/matt-riley/waffle/internal/redact"
)

// sanitizeDashboardString is the residual format scrubber for non-chat Desk
// projections (tasks, memory, workspaces). Chat events and chat HTTP results
// must not rely on these patterns as their secret boundary — they use
// ChatClients exact-value redaction and structural allowlisting instead
// (#153). The implementation is the shared residual scrubber from
// internal/redact, used by chatwire too (#289).
func sanitizeDashboardString(value string) string {
	return redact.Residual(value)
}
