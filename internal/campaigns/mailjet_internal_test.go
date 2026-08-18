package campaigns

import "testing"

// TestDefaultHTTPClientHasTimeout guards the actual regression: the
// behavioural timeout test supplies its own HTTPClient, so it would still
// pass if the default went back to http.DefaultClient (which has no
// deadline). This asserts the default a real dispatch uses is bounded.
func TestDefaultHTTPClientHasTimeout(t *testing.T) {
	client := &MailjetClient{APIKey: "key", APISecret: "secret"}

	if got := client.httpClient().Timeout; got <= 0 {
		t.Fatalf("default Mailjet HTTP client has no timeout (got %v) - a hung connection would strand the campaign in \"sending\"", got)
	}
}
