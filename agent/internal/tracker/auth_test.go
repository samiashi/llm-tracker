package tracker

import "testing"

func TestAuthRejectedMatchesOnlyARefusedCredential(t *testing.T) {
	for msg, want := range map[string]bool{
		"server returned 401: invalid token":  true,
		"server returned 403: ":               true,
		"server returned 500: internal error": false,
		// Push's own error for a refused batch, which a bare "401" misreads.
		"server refused 401 events and reported no usable retention floor":    false,
		`Post "http://10.0.0.1:8790/v1/ingest": dial tcp: connection refused`: false,
		"": false,
	} {
		if got := AuthRejected(msg); got != want {
			t.Errorf("AuthRejected(%q) = %v, want %v", msg, got, want)
		}
	}
}
