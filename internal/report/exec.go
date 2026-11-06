package report

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// run returns a host command's output, or the error in its place. Host facts
// are best effort: one that cannot be read says so and the report goes on.
func run(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "unknown (" + strings.TrimSpace(err.Error()) + ")"
	}
	return string(out)
}

// cpuShare reads a process's CPU time twice, a second apart, and returns the
// share of one CPU it used in between, in per cent.
func cpuShare(ctx context.Context, cpuTime func() (float64, error)) (float64, error) {
	before, err := cpuTime()
	if err != nil {
		return 0, err
	}
	start := time.Now()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(time.Second):
	}
	after, err := cpuTime()
	if err != nil {
		return 0, err
	}
	return (after - before) / time.Since(start).Seconds() * 100, nil
}

// parseClock reads [[hh:]mm:]ss[.frac] as seconds.
func parseClock(s string) (float64, error) {
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot read %q as a CPU time", s)
		}
		total = total*60 + v
	}
	return total, nil
}
