package main

import "syscall"

// posixACL reads a file's access ACL, or nil when it has none.
func posixACL(path string) []byte {
	n, err := syscall.Getxattr(path, "system.posix_acl_access", nil)
	if err != nil || n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	n, err = syscall.Getxattr(path, "system.posix_acl_access", buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}
