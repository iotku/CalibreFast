//go:build !(linux || darwin || freebsd)

package main

// statDisk can't tell disks apart here: the Server page shows what the
// server keeps, without free space.
func statDisk(string) (diskInfo, bool) { return diskInfo{}, false }
