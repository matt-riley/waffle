//go:build !linux

package netlock

// LockdownExceptHost is a no-op on non-Linux hosts (workspace runners are Linux).
func LockdownExceptHost(hostName string) error {
	return nil
}

// DropCapabilities is a no-op where Linux capabilities do not apply.
func DropCapabilities() error {
	return nil
}
