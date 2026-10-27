//go:build linux

package factory

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	watchRootMask = syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_ONLYDIR
	watchDirMask  = syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO
)

// watchDir reports agent files (<root>/<dir>/*.json) as they are written or renamed into place,
// including in directories made later. changed(nil) means the kernel dropped events. failed is
// called once if the watch breaks after it started; stop ends it.
func watchDir(root string, changed func([]string), failed func(error)) (stop func(), err error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify: %w", err)
	}
	file := os.NewFile(uintptr(fd), "inotify") // non-blocking: Close ends a pending Read
	dirs := map[int32]string{}
	add := func(dir string, mask uint32) error {
		wd, err := syscall.InotifyAddWatch(fd, dir, mask)
		if err != nil {
			return fmt.Errorf("watch %s: %w", filepath.Base(dir), err)
		}
		dirs[int32(wd)] = dir
		return nil
	}
	if err := add(root, watchRootMask); err != nil {
		file.Close()
		return nil, err
	}
	subs, err := os.ReadDir(root)
	if err != nil {
		file.Close()
		return nil, err
	}
	for _, e := range subs {
		if e.IsDir() {
			if err := add(filepath.Join(root, e.Name()), watchDirMask); err != nil {
				file.Close()
				return nil, err
			}
		}
	}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := file.Read(buf)
			if err != nil {
				if !errors.Is(err, os.ErrClosed) {
					failed(err)
				}
				return
			}
			var paths []string
			lost := false
			for off := 0; off+syscall.SizeofInotifyEvent <= n; {
				wd := int32(binary.NativeEndian.Uint32(buf[off:]))
				mask := binary.NativeEndian.Uint32(buf[off+4:])
				size := int(binary.NativeEndian.Uint32(buf[off+12:]))
				name := strings.TrimRight(string(buf[off+syscall.SizeofInotifyEvent:off+syscall.SizeofInotifyEvent+size]), "\x00")
				off += syscall.SizeofInotifyEvent + size
				dir, known := dirs[wd]
				switch {
				case mask&syscall.IN_Q_OVERFLOW != 0:
					lost = true
				case mask&syscall.IN_IGNORED != 0:
					delete(dirs, wd)
				case !known || name == "":
				case dir == root:
					if mask&syscall.IN_ISDIR == 0 {
						continue
					}
					sub := filepath.Join(root, name)
					if err := add(sub, watchDirMask); err != nil {
						lost = true // unwatched: the poll reads it
						continue
					}
					// files written before the watch was added
					found, _ := filepath.Glob(filepath.Join(sub, "*.json"))
					paths = append(paths, found...)
				case strings.HasSuffix(name, ".json"):
					paths = append(paths, filepath.Join(dir, name))
				}
			}
			if lost {
				changed(nil)
			}
			if len(paths) > 0 {
				changed(paths)
			}
		}
	}()
	return func() { file.Close() }, nil
}
