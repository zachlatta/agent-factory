//go:build !linux

package factory

import "errors"

// watchDir: no agent watcher off Linux; the loop polls only.
func watchDir(string, func([]string), func(error)) (func(), error) {
	return nil, errors.New("not supported on this system")
}
