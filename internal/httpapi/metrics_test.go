package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getMetrics fetches /metrics with the given token ("" for none) and
// returns the status and the text body.
func (s *testServer) getMetrics(token string) (int, string) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/metrics", nil)
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.srv.Client().Do(req)
	if err != nil {
		s.t.Fatalf("GET /metrics: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		s.t.Fatalf("read /metrics: %v", err)
	}
	return res.StatusCode, string(body)
}

// TestMetrics checks that requests and cache results are counted, that
// labels use route patterns rather than real paths, and that the endpoint
// needs an API key.
func TestMetrics(t *testing.T) {
	s := newDatabaseServer(t)
	s.mustCreate("secret-flag", true)
	s.wantCached("first", "secret-flag", "user-1", true, "MISS")
	s.wantCached("second", "secret-flag", "user-1", true, "HIT")
	s.do(http.MethodGet, "/v1/flags/missing", testReaderToken, nil) // a 404

	status, body := s.getMetrics(testReaderToken)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", status)
	}
	for _, want := range []string{
		`http_requests_total{route="POST /v1/flags",status="201"} 1`,
		`http_requests_total{route="GET /v1/flags/{name}/evaluate",status="200"} 2`,
		`http_requests_total{route="GET /v1/flags/{name}",status="404"} 1`,
		`http_request_duration_seconds_count{route="GET /v1/flags/{name}/evaluate"} 2`,
		`evaluation_cache_results_total{result="hit"} 1`,
		`evaluation_cache_results_total{result="miss"} 1`,
		`evaluation_cache_results_total{result="bypass"} 0`,
		`evaluation_cache_entries 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics are missing the line %q", want)
		}
	}

	// Flag names and user IDs must never appear as label values.
	if strings.Contains(body, "secret-flag") || strings.Contains(body, "user-1") {
		t.Errorf("metrics leak a flag name or user ID:\n%s", body)
	}

	if status, _ := s.getMetrics(""); status != http.StatusUnauthorized {
		t.Errorf("GET /metrics without a key: status %d, want 401", status)
	}
}
