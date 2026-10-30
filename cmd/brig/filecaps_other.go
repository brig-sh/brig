//go:build !linux

package main

// hasFileCaps has nothing to read off Linux, where the rows that ask do not run.
func hasFileCaps(string) bool { return false }
