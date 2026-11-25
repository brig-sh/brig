// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package nofireauth speaks the sign-in protocol of a NOFire AI deployment:
// RFC 8414 discovery, the PKCE loopback flow and the RFC 8628 device flow. It
// only talks. It never stores a token, a URL or any other state; the caller
// decides what to keep and where.
package nofireauth
