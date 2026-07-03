package secret

import "strings"

// minRedactLen guards against degenerate secrets ("1", "ok") turning the
// redactor into a text shredder. Anything shorter isn't much of a secret.
const minRedactLen = 6

// Redactor scrubs known secret values from text. The gateway runs every
// tool result, log line, and trace attribute through one of these so a
// stray `env` or `cat` can't leak a stored key into the transcript.
type Redactor struct {
	replacer *strings.Replacer
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
func NewRedactorWith(store Store, extras ...NamedValue) (*Redactor, error) {
	pairs := make([]string, 0, len(extras)*2)
	if store != nil {
		names, err := store.List()
		if err != nil {
			return nil, err
		}
		pairs = make([]string, 0, (len(names)+len(extras))*2)
		for _, name := range names {
			v, err := store.Get(name)
			if err != nil {
				return nil, err
			}
			pairs = appendRedactionPair(pairs, name, v)
		}
	}
	for _, extra := range extras {
		pairs = appendRedactionPair(pairs, extra.Name, extra.Value)
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}, nil
}

// Redact returns s with every known secret value replaced by its
// [redacted:name] marker.
func (r *Redactor) Redact(s string) string { return r.replacer.Replace(s) }

func appendRedactionPair(pairs []string, name, value string) []string {
	if len(value) < minRedactLen {
		return pairs
	}
	return append(pairs, value, "[redacted:"+name+"]")
}
