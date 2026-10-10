//go:build !windows

package main

import "syscall"

// restrictNewFiles makes every file this process creates private to the owner
func restrictNewFiles() {
	syscall.Umask(0077)
}
