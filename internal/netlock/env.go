package netlock

import (
	"fmt"
	"os"
	"strings"
)

// EnvLockdown is the environment variable workspace containers set so the
// runner must drop the default route (host broker only) before serving (#95).
const EnvLockdown = "WAFFLE_NET_LOCKDOWN"

// EnvLockdownHost names the host to keep reachable (default waffle-host).
const EnvLockdownHost = "WAFFLE_NET_LOCKDOWN_HOST"

// ApplyFromEnv reads lockdown env vars and, when lockdown is required, calls
// lockdown(host). Fail-closed: any lockdown error is returned so the runner
// must not continue with an open default route.
//
// getenv and lockdown are injectable for unit tests; production uses
// os.Getenv and LockdownExceptHost.
func ApplyFromEnv(getenv func(string) string, lockdown func(host string) error) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	if lockdown == nil {
		lockdown = LockdownExceptHost
	}
	v := strings.TrimSpace(getenv(EnvLockdown))
	if v != "1" && !strings.EqualFold(v, "true") {
		return nil
	}
	host := strings.TrimSpace(getenv(EnvLockdownHost))
	if host == "" {
		host = "waffle-host"
	}
	if err := lockdown(host); err != nil {
		return fmt.Errorf("net lockdown required for %s: %w", host, err)
	}
	return nil
}
