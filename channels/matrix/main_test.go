package main

import (
	"strings"
	"sync"
	"testing"
)

func TestExtractDisplayNameFromMXID(t *testing.T) {
	tests := []struct {
		mxid string
		want string
	}{
		{"@mybot:matrix.org", "mybot"},
		{"@alice:example.com", "alice"},
		{"@bob:matrix.example.org", "bob"},
		{"unknown", "unknown"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.mxid, func(t *testing.T) {
			got := extractDisplayNameFromMXID(tt.mxid)
			if got != tt.want {
				t.Errorf("extractDisplayNameFromMXID(%q) = %q, want %q", tt.mxid, got, tt.want)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	mc := &MatrixChannel{accessToken: "s3cret_token_123"}

	in := "GET https://matrix.org/_matrix/client/v3/sync?access_token=s3cret_token_123: timeout"
	out := mc.redactError(in)

	if strings.Contains(out, "s3cret_token_123") {
		t.Errorf("redactError left the access token in the string: %q", out)
	}
	if !strings.Contains(out, "***REDACTED***") {
		t.Errorf("redactError did not insert the redaction marker: %q", out)
	}
	if !strings.Contains(out, "timeout") {
		t.Errorf("redactError removed non-secret content: %q", out)
	}
}

func TestRedactErrorEmpty(t *testing.T) {
	mc := &MatrixChannel{accessToken: ""}
	in := "some error"
	if got := mc.redactError(in); got != in {
		t.Errorf("redactError with empty token = %q, want unchanged %q", got, in)
	}
}

func TestNextTxnID(t *testing.T) {
	var txnMu sync.Mutex
	var lastTxnID int64

	// First call should return 1.
	id1 := nextTxnID(&txnMu, &lastTxnID)
	if id1 != 1 {
		t.Errorf("first txnID = %d, want 1", id1)
	}

	// Second call should return 2.
	id2 := nextTxnID(&txnMu, &lastTxnID)
	if id2 != 2 {
		t.Errorf("second txnID = %d, want 2", id2)
	}

	// IDs should be monotonically increasing.
	for i := 0; i < 100; i++ {
		id := nextTxnID(&txnMu, &lastTxnID)
		if id <= id2 {
			t.Errorf("txnID not monotonically increasing: got %d after %d", id, id2)
		}
		lastTxnID = id
	}
}
