package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorovista/internal/source"
	"github.com/sorotrail/sorovista/internal/source/sorotrailsource"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// probeServer mirrors cmd/sorovista: the probes live at the root, outside
// the /api tree that Routes() returns.
func probeServer(status source.Status) http.Handler {
	s := New(&fakeSource{status: status}, discardLogger())
	mux := http.NewServeMux()
	mux.Handle("/livez", s.LivezHandler())
	mux.Handle("/readyz", s.ReadyzHandler())
	return mux
}

// livez must stay 200 while the backend is down: a liveness probe that
// fails on dependencies makes an orchestrator restart-loop a recoverable
// instance instead of pulling it from the load balancer.
func TestLivezHealthyWhenBackendDown(t *testing.T) {
	srv := httptest.NewServer(probeServer(source.Status{
		Mode:    "rpc",
		Healthy: false,
		Detail:  "connection refused",
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("livez with backend down = %d, want 200", res.StatusCode)
	}
}

func TestReadyzHealthy(t *testing.T) {
	srv := httptest.NewServer(probeServer(source.Status{
		Mode:         "rpc",
		Healthy:      true,
		LatestLedger: 1234,
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz with healthy source = %d, want 200", res.StatusCode)
	}
}

func TestReadyzReportsBackendFailure(t *testing.T) {
	srv := httptest.NewServer(probeServer(source.Status{
		Mode:    "sorotrail",
		Healthy: false,
		Detail:  "indexer unreachable",
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unhealthy source = %d, want 503", res.StatusCode)
	}
}

// TestReadyzAgainstUnreachableUpstream wires a real upstream-mode source, so
// the mapping from sorotrailsource.Status to the probe's status code is pinned
// end to end: an indexer that cannot be reached makes this instance not ready,
// and the reason is handed to the operator rather than hidden behind the code.
func TestReadyzAgainstUnreachableUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstream.Close() // a closed listener is a genuine connection refusal

	s := New(sorotrailsource.New(upstream.URL, upstream.Client()), discardLogger())
	mux := http.NewServeMux()
	mux.Handle("/readyz", s.ReadyzHandler())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"an unreachable indexer is what readiness is meant to detect")

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "not ready", body["status"])
	assert.Equal(t, "sorotrail", body["mode"])
	assert.NotEmpty(t, body["detail"], "a 503 with no explanation leaves an operator guessing")
}

func TestVersionEndpoint(t *testing.T) {
	// Built through the api tree like the other endpoints; pins the real
	// values, so a broken ldflags path shows up as "dev"/"none".
	srv := httptest.NewServer(New(&fakeSource{}, discardLogger()).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("version = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "commit", "build_date"} {
		if body[key] == "" {
			t.Fatalf("version response missing %q: %+v", key, body)
		}
	}
}
