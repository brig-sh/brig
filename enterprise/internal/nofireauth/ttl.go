// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ParseTTL reads a token lifetime such as 12h or 30d. Only hours and days are
// accepted, as plain positive integers, so the value converts to the integer
// seconds the server takes with no rounding.
func ParseTTL(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, errors.New("--ttl must be a number followed by h or d, such as 12h or 30d")
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, fmt.Errorf("--ttl %q must end in h or d, such as 12h or 30d", s)
	}
	digits := s[:len(s)-1]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, fmt.Errorf("--ttl %q must be a whole number followed by h or d, such as 12h or 30d", s)
		}
	}
	n, err := strconv.ParseUint(digits, 10, 63)
	if err != nil || n > uint64(math.MaxInt64/int64(unit)) {
		return 0, fmt.Errorf("--ttl %q is too large. Use a smaller number", s)
	}
	if n == 0 {
		return 0, fmt.Errorf("--ttl %q must be greater than zero. Use a number such as 12h", s)
	}
	return time.Duration(n) * unit, nil
}
