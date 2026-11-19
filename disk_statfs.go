//go:build linux || darwin || freebsd

package main

import "syscall"

// statDisk is the filesystem holding p (which must exist) and the room left on it.
func statDisk(p string) (diskInfo, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return diskInfo{}, false
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(p, &fs); err != nil {
		return diskInfo{}, false
	}
	bs := int64(fs.Bsize)
	return diskInfo{ID: uint64(st.Dev), Total: int64(fs.Blocks) * bs, Free: int64(fs.Bavail) * bs}, true
}
