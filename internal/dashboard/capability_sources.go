package dashboard

import (
	"path/filepath"
	"sort"
	"strings"
)

// CapabilitySkillSources contains only labels that help choose an allowed
// installer source. Absolute filesystem paths never cross the Desk boundary.
type CapabilitySkillSources struct {
	LocalRoots []string `json:"local_roots"`
	GitHosts   []string `json:"git_hosts"`
}

// NewCapabilitySkillSources converts configured source policy into safe public
// labels while retaining the deny-by-default empty state.
func NewCapabilitySkillSources(importRoots, gitHosts []string) CapabilitySkillSources {
	return CapabilitySkillSources{
		LocalRoots: capabilitySourceLabels(importRoots, func(value string) (string, bool) {
			value = filepath.Base(filepath.Clean(strings.TrimSpace(value)))
			if value == "." || value == string(filepath.Separator) || value == "" {
				return "", false
			}
			return value, true
		}),
		GitHosts: capabilitySourceLabels(gitHosts, func(value string) (string, bool) {
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "" || strings.ContainsAny(value, "/:@") {
				return "", false
			}
			for _, character := range value {
				if (character < 'a' || character > 'z') &&
					(character < '0' || character > '9') && character != '.' && character != '-' {
					return "", false
				}
			}
			return value, true
		}),
	}
}

func capabilitySourceLabels(values []string, normalize func(string) (string, bool)) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		label, ok := normalize(value)
		if !ok {
			continue
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}
