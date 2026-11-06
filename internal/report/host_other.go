//go:build !darwin && !linux

package report

import (
	"context"
	"errors"
)

func hostFacts(context.Context) string { return "host          not described on this OS\n" }

func procStats(context.Context, int) (string, float64, error) {
	return "", 0, errors.New("not supported on this OS")
}

func vmmStacks(context.Context, int) (string, string) { return "", "not supported on this OS" }
