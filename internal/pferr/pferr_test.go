package pferr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// The three public codes and how any error is turned into one of them.
func TestPublic(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
	}{
		{"parse", Parsef("the data was broken at byte %d", 7), "E_PARSE: the data was broken at byte 7"},
		{"needs subtool", Needsf("valid and unfamiliar"), "E_NEEDS_SUBTOOL: valid and unfamiliar"},
		{"timeout", Timeoutf("too slow"), "E_TIMEOUT: too slow"},
		{"wrapped public error", fmt.Errorf("while reading: %w", Needsf("valid and unfamiliar")), "E_NEEDS_SUBTOOL: valid and unfamiliar"},
		{"deadline from the network", fmt.Errorf("read tcp 192.0.2.1:80: %w", os.ErrDeadlineExceeded), "E_TIMEOUT: the request took too long"},
		{"deadline from a context", context.DeadlineExceeded, "E_TIMEOUT: the request took too long"},
		{"anything else hides its detail", errors.New("open /data/secret on host-7: permission denied"), "E_PARSE: fallback sentence"},
	}
	for _, c := range cases {
		if got := Public(c.in, "fallback sentence").Error(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
