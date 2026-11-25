// Package mcpproxy decides what an MCP server may be asked, and what it may
// answer, for the broker that stands between the guest and a remote MCP
// server. It is pure: no network and no credential. The broker owns both and
// calls in with bytes.
//
// It must not import internal/broker or internal/memory. The broker depends on
// this package, and the memory server is a separate MCP surface with its own
// token; a dependency either way would let one surface's rules leak into the
// other. arch_test.go enforces it.
package mcpproxy
