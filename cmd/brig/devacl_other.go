//go:build !linux

package main

// posixACL has nothing to read off Linux, where the rows that ask do not run.
func posixACL(string) []byte { return nil }
