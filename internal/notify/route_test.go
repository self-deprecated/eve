package notify

import (
	"fmt"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

func TestAllowedURL(t *testing.T) {
	allow := []string{"*.tail6e682f.ts.net", "hooks.example.com", "127.0.0.1:9000", "100.101.102.103"}
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://hooks.example.com/eve", true},
		{"https://HOOKS.example.com/eve", true},
		{"https://quartz.tail6e682f.ts.net/hook", true},
		{"http://quartz.tail6e682f.ts.net:8080/hook", true},    // http on the tailnet
		{"http://127.0.0.1:9000/hook", true},                   // loopback, port-specific pattern
		{"http://100.101.102.103/hook", true},                  // tailnet CGNAT address
		{"http://127.0.0.1:9001/hook", false},                  // port not allowed
		{"http://hooks.example.com/eve", false},                // plain http off the tailnet
		{"https://user:pw@hooks.example.com/eve", false},       // userinfo
		{"https://evil.example/eve", false},                    // not allow-listed
		{"https://hooks.example.com.evil.example/", false},     // suffix trick
		{"https://a.tail6e682f.ts.net.evil.example/", false},   // suffix trick on the glob
		{"https://other.tailffffff.ts.net/", false},            // another tailnet
		{"ftp://hooks.example.com/", false},                    // scheme
		{"hooks.example.com/eve", false},                       // no scheme, no host
		{"https:///path", false},                               // empty host
		{"https://[::1]/x", false},                             // not allow-listed
		{"http://[fd7a:115c:a1e0::1]/x", false},                // tailnet, but not allow-listed
		{"https://hooks.example.com:8443/eve?token=abc", true}, // port free when pattern has none
		{"https://quartz.tail6e682f.ts.net/" + "%zz", false},   // unparsable
		{"javascript:alert(1)", false},                         // opaque
		{"https://hooks.example.com@evil.example/", false},     // userinfo trick
		{"http://localhost:9000/x", false},                     // loopback, not allow-listed
		{"https://sub.quartz.tail6e682f.ts.net/", true},        // glob spans labels
		{"http://quartz.tail6e682f.ts.net.:80/", false},        // trailing dot is another name
		{"http://[::ffff:100.101.102.103]/", false},            // mapped form is not the pattern
		{"https://100.101.102.103/", true},                     // https to an allowed IP
		{"https://127.0.0.1/", false},                          // port-less loopback not listed
	} {
		t.Run(tc.url, func(t *testing.T) {
			err := AllowedURL(tc.url, allow)
			if (err == nil) != tc.ok {
				t.Errorf("AllowedURL(%q) = %v, want ok=%v", tc.url, err, tc.ok)
			}
		})
	}
	if err := AllowedURL("https://hooks.example.com/", nil); err == nil {
		t.Error("empty allowHosts must reject everything")
	}
}

func TestBuildDeliveries(t *testing.T) {
	human := &config.HumanHook{URL: "https://grok.example/hook"}
	urls := []string{"https://hooks.example.com/a", "https://hooks.example.com/a", " "}
	agents := []Target{{KindWebhook, "https://hooks.example.com/a"}}
	withHuman := append(append([]Target{}, agents...), Target{KindHuman, human.URL})

	for _, tc := range []struct {
		event string
		human *config.HumanHook
		want  []Target
	}{
		{api.NotifySucceeded, human, agents},
		{api.NotifyFailed, human, agents},
		{api.NotifyRejected, human, agents},
		{api.NotifyCancelled, human, agents},
		{api.NotifyInterrupted, human, agents},
		{api.NotifyUnavailable, human, agents},
		{api.NotifyAwaitingConfirmation, human, agents}, // never the human
		{api.NotifyNeedsHuman, human, withHuman},
		{api.NotifyWaitingForDevice, human, withHuman},
		{api.NotifyNeedsHuman, nil, agents},
	} {
		t.Run(fmt.Sprintf("%s/human=%v", tc.event, tc.human != nil), func(t *testing.T) {
			got := BuildDeliveries(api.Notification{Event: tc.event}, urls, tc.human, "")
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	// The explicit event argument wins over n.Event.
	got := BuildDeliveries(api.Notification{Event: api.NotifySucceeded}, nil, human, api.NotifyNeedsHuman)
	if len(got) != 1 || got[0].Kind != KindHuman {
		t.Errorf("explicit event: %v", got)
	}
}

func TestExhaustedCopy(t *testing.T) {
	human := &config.HumanHook{URL: "https://grok.example/hook"}
	if tg, ok := ExhaustedCopy(Delivery{Target: Target{KindWebhook, "https://hooks.example.com/a"}}, human); !ok || tg != (Target{KindHuman, human.URL}) {
		t.Errorf("t3 exhausted: %v %v", tg, ok)
	}
	if _, ok := ExhaustedCopy(Delivery{Target: Target{KindHuman, human.URL}}, human); ok {
		t.Error("a human delivery must not be copied to the human")
	}
	if _, ok := ExhaustedCopy(Delivery{Target: Target{KindWebhook, "u"}}, nil); ok {
		t.Error("no hook configured")
	}
}

func TestConfigSecrets(t *testing.T) {
	cs := ConfigSecrets{
		Notify: config.Notify{
			Human: &config.HumanHook{URL: "https://grok.example/hook", Secret: "env:H"},
			Secrets: []config.HookToken{
				{Prefix: "https://hooks.example.com/", Secret: "env:A"},
				{Prefix: "https://hooks.example.com/team/", Secret: "env:B"},
			},
		},
		Resolve: func(ref string) (string, error) { return ref, nil },
	}
	for _, tc := range []struct {
		target Target
		want   string
	}{
		{Target{KindWebhook, "https://hooks.example.com/x"}, "env:A"},
		{Target{KindWebhook, "https://hooks.example.com/team/x"}, "env:B"},
		{Target{KindWebhook, "https://other.example/"}, ""},
		{Target{KindHuman, "https://grok.example/hook"}, "env:H"},
		{Target{KindHuman, "https://changed.example/"}, ""},
	} {
		got, err := cs.Secret(tc.target)
		if err != nil || got != tc.want {
			t.Errorf("Secret(%v) = %q, %v; want %q", tc.target, got, err, tc.want)
		}
	}
}
