//go:build !darwin && !linux

package skillinstall

func atomicRenameNoReplace(string, string) error {
	return ErrAtomicRenameUnsupported
}
