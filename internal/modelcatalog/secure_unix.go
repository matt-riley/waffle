//go:build unix

package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type secureCacheDir struct {
	file *os.File
	fd   int
}

func openSecureCacheDir(path string) (*secureCacheDir, error) {
	directory, err := openCacheDirHandle(path)
	if err != nil {
		return nil, err
	}
	if err := directory.validate(); err != nil {
		_ = directory.close()
		return nil, err
	}
	return directory, nil
}

func secureCacheRoot(path string) (*secureCacheDir, error) {
	directory, err := openCacheDirHandle(path)
	if err != nil {
		return nil, err
	}
	if err := directory.validateOwnerAndType(); err != nil {
		_ = directory.close()
		return nil, err
	}
	if err := unix.Fchmod(directory.fd, 0o700); err != nil {
		_ = directory.close()
		return nil, err
	}
	if err := directory.validate(); err != nil {
		_ = directory.close()
		return nil, err
	}
	return directory, nil
}

func openCacheDirHandle(path string) (*secureCacheDir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create file handle for model catalogue cache directory")
	}
	directory := &secureCacheDir{file: file, fd: fd}
	return directory, nil
}

func (d *secureCacheDir) validate() error {
	if err := d.validateOwnerAndType(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(d.fd, &stat); err != nil {
		return err
	}
	if stat.Mode&0o7777 != 0o700 {
		return fmt.Errorf("model catalogue cache root mode is %04o, want 0700", stat.Mode&0o7777)
	}
	return nil
}

func (d *secureCacheDir) validateOwnerAndType() error {
	var stat unix.Stat_t
	if err := unix.Fstat(d.fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("model catalogue cache root is not a directory")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("model catalogue cache root is owned by uid %d, want %d", stat.Uid, os.Geteuid())
	}
	return nil
}

func (d *secureCacheDir) close() error {
	return d.file.Close()
}

func (d *secureCacheDir) openRegular(name string) (*os.File, cacheGeneration, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, cacheGeneration{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, cacheGeneration{}, errors.New("create file handle for model catalogue cache")
	}
	stat, err := validateOwnedRegularFile(fd, true)
	if err != nil {
		_ = file.Close()
		return nil, cacheGeneration{}, err
	}
	return file, generationFromStat(stat), nil
}

func (d *secureCacheDir) validateMutationTarget(name string) error {
	stat, err := d.lstat(name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("model catalogue cache target is not a regular file")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("model catalogue cache target is owned by uid %d, want %d", stat.Uid, os.Geteuid())
	}
	return nil
}

func (d *secureCacheDir) validateTemporary(file *os.File, name string) (cacheGeneration, error) {
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return cacheGeneration{}, err
	}
	linked, err := d.lstat(name)
	if err != nil {
		return cacheGeneration{}, err
	}
	if opened.Dev != linked.Dev || opened.Ino != linked.Ino {
		return cacheGeneration{}, errors.New("model catalogue cache staging file is outside the secured directory")
	}
	_, err = validateOwnedRegularFile(int(file.Fd()), true)
	if err != nil {
		return cacheGeneration{}, err
	}
	return generationFromStat(&opened), nil
}

func (d *secureCacheDir) commitTemporary(temporary, destination string, expected cacheGeneration) (bool, error) {
	if err := d.validateMutationTarget(destination); err != nil {
		return false, err
	}
	temporaryStat, err := d.lstat(temporary)
	if err != nil {
		return false, err
	}
	if temporaryStat.Mode&unix.S_IFMT != unix.S_IFREG || int(temporaryStat.Uid) != os.Geteuid() || temporaryStat.Mode&0o7777 != 0o600 {
		return false, errors.New("model catalogue cache staging target is not an owned regular 0600 file")
	}
	if generationFromStat(temporaryStat) != expected {
		return false, errors.New("model catalogue cache staging target changed before commit")
	}
	if err := unix.Renameat(d.fd, temporary, d.fd, destination); err != nil {
		return false, err
	}
	if err := unix.Fsync(d.fd); err != nil {
		return true, err
	}
	return true, nil
}

func (d *secureCacheDir) removeRegular(name string) (bool, error) {
	if err := d.validateMutationTarget(name); err != nil {
		return false, err
	}
	if _, err := d.lstat(name); errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		return false, err
	}
	if err := unix.Fsync(d.fd); err != nil {
		return true, err
	}
	return true, nil
}

func (d *secureCacheDir) lstat(name string) (*unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	return &stat, nil
}

func acquireRefreshLock(ctx context.Context, directory *secureCacheDir, name string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := openRefreshLock(ctx, directory.fd, name)
	if err != nil {
		return nil, fmt.Errorf("open refresh lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create file handle for model catalogue refresh lock")
	}
	if _, err := validateOwnedRegularFile(fd, false); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("validate refresh lock: %w", err)
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure refresh lock: %w", err)
	}

	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(fd, unix.LOCK_UN), file.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func openRefreshLock(ctx context.Context, directoryFD int, name string) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		fd, err := unix.Openat(directoryFD, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.ENOENT) {
			return -1, err
		}
		fd, err = unix.Openat(directoryFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return -1, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateOwnedRegularFile(fd int, requireExactMode bool) (*unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("model catalogue cache path is not a regular file")
	}
	if int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("model catalogue cache path is owned by uid %d, want %d", stat.Uid, os.Geteuid())
	}
	if requireExactMode && stat.Mode&0o7777 != 0o600 {
		return nil, fmt.Errorf("model catalogue cache file mode is %04o, want 0600", stat.Mode&0o7777)
	}
	return &stat, nil
}

func generationFromStat(stat *unix.Stat_t) cacheGeneration {
	return cacheGeneration{identity: fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)}
}
