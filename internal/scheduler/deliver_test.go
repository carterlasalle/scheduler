package scheduler

import (
	"errors"
	"testing"
)

func TestIsRetryableSendError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		out    string
		want   bool
	}{
		{"telegram timed out", errors.New("exit status 1"), "hermes send: Telegram send failed: Timed out", true},
		{"connection reset", errors.New("exit status 1"), "connection reset by peer", true},
		{"503 backend", errors.New("exit status 1"), "503 Service Unavailable", true},
		{"429 rate limited", errors.New("exit status 1"), "HTTP 429", true},
		{"i/o timeout", errors.New("exit status 1"), "i/o timeout", true},
		{"unknown platform", errors.New("exit status 2"), "unknown platform: foo", false},
		{"missing --to", errors.New("exit status 2"), "--to PLATFORM is required", false},
		{"parse error", errors.New("exit status 2"), "invalid argument", false},
	}
	for _, c := range cases {
		if got := isRetryableSendError(c.err, c.out); got != c.want {
			t.Errorf("%s: isRetryableSendError = %v, want %v", c.name, got, c.want)
		}
	}
}
