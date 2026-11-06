//go:build !unix

package report

// alive cannot ask about a process here, so it never reports a gateway down.
func alive(int) bool { return true }
