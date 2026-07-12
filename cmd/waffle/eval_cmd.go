package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/eval"
)

// evalCmd runs the zero-network eval harness (#63). Exits 1 on any failure
// via main's os.Exit when this returns a non-nil error.
func evalCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = stderr
	if len(args) > 0 {
		return fmt.Errorf("usage: waffle eval")
	}
	fails := eval.RunAll(ctx, stdout, eval.Registry())
	if fails > 0 {
		return fmt.Errorf("%d eval case(s) failed", fails)
	}
	return nil
}
