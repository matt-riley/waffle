package secret

import (
	"sort"
	"strings"
)

// minRedactLen guards against degenerate secrets ("1", "ok") turning the
// redactor into a text shredder. Anything shorter isn't much of a secret.
const minRedactLen = 6

// Redactor scrubs known secret values from text. The gateway runs every
// tool result, log line, and trace attribute through one of these so a
// stray `env` or `cat` can't leak a stored key into the transcript.
type Redactor struct {
	replacer *strings.Replacer
	// maxLen is the longest enrolled secret value (see MaxLen).
	maxLen int
}

// NewRedactor builds a Redactor over every value currently in the store.
// Rebuild after mutations; for phase 0 scale (a handful of secrets) that is
// perfectly cheap.
func NewRedactor(store Store) (*Redactor, error) {
	return NewRedactorWith(store)
}

// NamedValue is an extra secret value that should be redacted under name
// even when it did not come from the secret store.
type NamedValue struct {
	Name  string
	Value string
}

// NewRedactorWith builds a Redactor over every value currently in the store
// plus any explicitly supplied runtime-only secret values.
//
// Replacement pairs are ordered longest-value-first so strings.Replacer
// prefers the most specific match when one secret value is a prefix of
// another (see #98). Replacer otherwise uses first-listed-wins, which can
// leave a longer secret's suffix unredacted if a shorter prefix was listed
// first (e.g. via alphabetical secret-name order from store.List).
func NewRedactorWith(store Store, extras ...NamedValue) (*Redactor, error) {
	// Collect (name, value) candidates, then sort by value length descending
	// before building the flat old/new pair list for strings.NewReplacer.
	type candidate struct {
		name  string
		value string
	}
	var cands []candidate

	if store != nil {
		names, err := store.List()
		if err != nil {
			return nil, err
		}
		cands = make([]candidate, 0, len(names)+len(extras))
		for _, name := range names {
			v, err := store.Get(name)
			if err != nil {
				return nil, err
			}
			if len(v) < minRedactLen {
				continue
			}
			cands = append(cands, candidate{name: name, value: v})
		}
	} else {
		cands = make([]candidate, 0, len(extras))
	}
	for _, extra := range extras {
		if len(extra.Value) < minRedactLen {
			continue
		}
		cands = append(cands, candidate{name: extra.Name, value: extra.Value})
	}

	// Longest value first so prefix secrets cannot partially redact a longer
	// secret that contains them as a prefix.
	sort.SliceStable(cands, func(i, j int) bool {
		return len(cands[i].value) > len(cands[j].value)
	})

	pairs := make([]string, 0, len(cands)*2)
	maxLen := 0
	for _, c := range cands {
		pairs = append(pairs, c.value, "[redacted:"+c.name+"]")
		if len(c.value) > maxLen {
			maxLen = len(c.value)
		}
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...), maxLen: maxLen}, nil
}

// Redact returns s with every known secret value replaced by its
// [redacted:name] marker.
func (r *Redactor) Redact(s string) string { return r.replacer.Replace(s) }

// MaxLen returns the length of the longest enrolled secret value, or 0 when
// the redactor knows no values. Streaming redaction (e.g. broker response
// bodies) retains this many tail bytes so a credential can never straddle a
// flush boundary unseen; callers fall back to a sane default when it is 0.
func (r *Redactor) MaxLen() int {
	return r.maxLen
}
