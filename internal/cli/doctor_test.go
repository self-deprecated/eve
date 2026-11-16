package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
)

// A newer CLI beside an eve serve nobody restarted (same API level) must not pass silently:
// that is how Quartz ran a server without Simulator deletion for a week.
func TestDoctorWarnsWhenServerBuildDiffersFromClient(t *testing.T) {
	for _, tc := range []struct{ client, server, want string }{
		{"0.1.0+55d1c6e", "0.1.0+670b989", "the server runs eve 0.1.0+670b989 but this client is 0.1.0+55d1c6e"},
		{"0.1.0+55d1c6e", "0.1.0+55d1c6e", ""},
		{"test", "0.1.0+670b989", ""}, // development builds carry no commit to compare
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(api.VersionInfo{Version: tc.server, API: api.APILevel})
		}))
		root, env := Root(tc.client)
		var out, errOut bytes.Buffer
		env.Out, env.Err = &out, &errOut
		env.NewClient = func() (*client.Client, error) { return client.New(srv.URL, "tok"), nil }
		root.SetArgs([]string{"doctor"})
		err := root.Execute()
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := errOut.String(); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Fatalf("client %s, server %s: stderr %q, want %q", tc.client, tc.server, got, tc.want)
		}
	}
}
