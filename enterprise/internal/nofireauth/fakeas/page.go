// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package fakeas

import (
	"fmt"
	"html"
	"os"
	"regexp"
)

var refresh = regexp.MustCompile(`<meta http-equiv=refresh content="0;url=([^"]*)">`)

// FollowPage reads the page that brig login hands its opener, and returns
// the URL the page redirects to, as a browser that opened the file would.
func FollowPage(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := refresh.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("%s has no refresh to follow", path)
	}
	return html.UnescapeString(string(m[1])), nil
}
