package main

import "testing"

func TestEmptyStateThemeDocumentStartUsesApprovedLiterals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested string
		shell     bool
		want      string
	}{
		{
			name:      "light shell",
			requested: "light",
			shell:     true,
			want:      `<!doctype html><html lang="en" data-theme="light" data-theme-preference="light">`,
		},
		{
			name:      "dark shell",
			requested: "dark",
			shell:     true,
			want:      `<!doctype html><html lang="en" data-theme="dark" data-theme-preference="dark">`,
		},
		{
			name:      "light standalone",
			requested: "light",
			want:      `<!doctype html><html lang="en" data-theme="light">`,
		},
		{
			name:      "dark standalone",
			requested: "dark",
			want:      `<!doctype html><html lang="en" data-theme="dark">`,
		},
		{
			name:      "malicious shell query",
			requested: `"><script>alert("theme")</script>`,
			shell:     true,
			want:      `<!doctype html><html lang="en" data-theme="light" data-theme-preference="light">`,
		},
		{
			name:      "malicious standalone query",
			requested: `"><img src=x onerror=alert("theme")>`,
			want:      `<!doctype html><html lang="en" data-theme="light">`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := emptyStateThemeDocumentStart(test.requested, test.shell); got != test.want {
				t.Fatalf("emptyStateThemeDocumentStart(%q, %t) = %q, want approved literal %q", test.requested, test.shell, got, test.want)
			}
		})
	}
}
