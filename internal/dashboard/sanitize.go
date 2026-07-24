package dashboard

import (
	"regexp"
	"strings"
)

// Residual format scrubbing for non-chat Desk projections (tasks, memory,
// workspaces). Chat events and chat HTTP results must not rely on these
// patterns as their secret boundary — they use ChatClients exact-value
// redaction and structural allowlisting instead (#153).
var dashboardSensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`AGE-SECRET-KEY-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`/var/lib/waffle(?:/[A-Za-z0-9._/-]+)?`),
}

func sanitizeDashboardString(value string) string {
	clean := strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
	for _, pattern := range dashboardSensitivePatterns {
		clean = pattern.ReplaceAllString(clean, "[redacted]")
	}
	return clean
}
