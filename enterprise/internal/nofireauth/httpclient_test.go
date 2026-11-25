// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The client every call to a deployment uses does not follow a redirect: the
// 307 is the answer, and the host it names is never contacted, with or
// without the bearer token the request carried.
func TestNewHTTPClientDoesNotFollowARedirect(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer home.Close()

	c := NewHTTPClient(time.Second)
	req, _ := http.NewRequest(http.MethodPost, home.URL, nil)
	req.Header.Set("Authorization", "Bearer sentinel")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status %d, want the 307 itself", resp.StatusCode)
	}
	if elsewhere.Load() != 0 {
		t.Error("the redirect was followed")
	}
	if c.Timeout != time.Second {
		t.Errorf("timeout %v, want 1s", c.Timeout)
	}
}
