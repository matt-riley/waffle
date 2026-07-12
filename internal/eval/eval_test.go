package eval

import (
	"bytes"
	"context"
	"testing"
)

func TestRegistryPassesOffline(t *testing.T) {
	var buf bytes.Buffer
	fails := RunAll(context.Background(), &buf, Registry())
	if fails != 0 {
		t.Fatalf("fails=%d\n%s", fails, buf.String())
	}
}
