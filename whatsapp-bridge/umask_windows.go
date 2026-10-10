//go:build windows

package main

// restrictNewFiles does nothing on Windows, where files inherit the ACL of the store directory
func restrictNewFiles() {}
