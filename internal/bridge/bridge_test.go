package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/run"
)

// Captured on Quartz with the bridge running: dns-sd repeats records, and the bridge's own
// advert has to be left out.
const zone = `Browsing for _remotepairing._tcp.local.
DATE: ---Tue 27 Oct 2026---
12:46:59.496  ...STARTING...

; To direct clients to browse a different domain, substitute that domain in place of '@'
lb._dns-sd._udp                                 PTR     @

_remotepairing._tcp                                PTR     EC2FAA5B-EAFA-49E9-9432-2DE0198373E6._remotepairing._tcp
EC2FAA5B-EAFA-49E9-9432-2DE0198373E6._remotepairing._tcp SRV     0 0 49152 Jordans-iPhone.local. ; Replace with unicast FQDN of target host
EC2FAA5B-EAFA-49E9-9432-2DE0198373E6._remotepairing._tcp TXT     "identifier=EC2FAA5B-EAFA-49E9-9432-2DE0198373E6" "authTag=Isqdo+Gx" "ver=26" "minVer=8" "flags=0"

_remotepairing._tcp                                PTR     Other\032Phone._remotepairing._tcp
Other\032Phone._remotepairing._tcp                 SRV     0 0 49152 eve-bridge.local. ; Replace with unicast FQDN of target host
Other\032Phone._remotepairing._tcp                 TXT     "identifier=X" "authTag=Y"

EC2FAA5B-EAFA-49E9-9432-2DE0198373E6._remotepairing._tcp TXT     "identifier=EC2FAA5B-EAFA-49E9-9432-2DE0198373E6" "authTag=new\"tag" "ver=26"
`

func TestParseZone(t *testing.T) {
	seen := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	got := ParseZone(zone, seen)
	if len(got) != 1 {
		t.Fatalf("adverts = %+v, want only the phone's", got)
	}
	a := got[0]
	if a.Name != "EC2FAA5B-EAFA-49E9-9432-2DE0198373E6" || !a.SeenAt.Equal(seen) {
		t.Errorf("advert = %+v", a)
	}
	if want := []string{"identifier=EC2FAA5B-EAFA-49E9-9432-2DE0198373E6", `authTag=new"tag`, "ver=26"}; !slices.Equal(a.TXT, want) {
		t.Errorf("TXT = %q, want the last copy %q", a.TXT, want)
	}
	if ParseZone("Browsing for _remotepairing._tcp.local.\n", seen) != nil {
		t.Error("no adverts should parse as none")
	}
	if got := unescape(`Other\032Phone\.x`); got != "Other Phone.x" {
		t.Errorf("unescape = %q", got)
	}
}

func TestTunnelEndpoint(t *testing.T) {
	line := `2026-10-27 12:36:09.619 Df remotepairingd[21863:97857e5] [com.apple.dt.remotepairing:networktunnelmanager] tunnel-97: Got tunnel endpoint: '192.168.0.103%en0:60822', includePeerToPeer: false`
	ip, port, ok := tunnelEndpoint(line)
	if !ok || ip.String() != "192.168.0.103" || port != 60822 {
		t.Fatalf("tunnelEndpoint = %v %d %v", ip, port, ok)
	}
	if ip, port, ok := tunnelEndpoint(`Got tunnel endpoint: 'fe80::1%en0:50000', x`); !ok || ip.String() != "fe80::1" || port != 50000 {
		t.Errorf("IPv6 = %v %d %v", ip, port, ok)
	}
	for _, l := range []string{"Got tunnel endpoint\"\"", "Got tunnel endpoint: '192.168.0.103%en0:49152'", "nothing"} {
		if _, _, ok := tunnelEndpoint(l); ok {
			t.Errorf("%q parsed", l)
		}
	}
}

func TestWrap(t *testing.T) {
	for in, want := range map[int]int{49153: 49153, 65535: 65535, 65536: 49153, 65540: 49157} {
		if got := wrap(in); got != want {
			t.Errorf("wrap(%d) = %d, want %d", in, got, want)
		}
	}
}

// upstream is a fake phone: it echoes on every port it listens on.
func upstream(t *testing.T, host string, ports ...int) {
	t.Helper()
	for _, p := range ports {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err != nil {
			t.Skipf("can't listen on %s:%d: %v", host, p, err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = c.Close() }()
					_, _ = io.Copy(c, c)
				}()
			}
		}()
	}
}

// loopbackPair returns the build Mac's and the phone's addresses for relay tests: two loopback
// addresses, which only Linux has without configuration.
func loopbackPair(t *testing.T) (net.IP, string) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a second loopback address (127.0.0.2)")
	}
	return net.ParseIP("127.0.0.1").To4(), "127.0.0.2"
}

// freeWindow finds windowSize consecutive free ports on both addresses.
func freeWindow(t *testing.T, local, phone string) int {
	for base := 60000; base < 65000; base += windowSize {
		ok := true
		for p := base; p < base+windowSize && ok; p++ {
			for _, h := range []string{local, phone} {
				l, err := net.Listen("tcp", net.JoinHostPort(h, strconv.Itoa(p)))
				if err != nil {
					ok = false
					break
				}
				_ = l.Close()
			}
		}
		if ok {
			return base
		}
	}
	t.Skip("no free port window")
	return 0
}

func echo(t *testing.T, from net.IP, to string) error {
	t.Helper()
	d := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: from}}
	c, err := d.Dial("tcp", to)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		return err
	}
	if string(buf) != "ping" {
		return errors.New("wrong echo " + string(buf))
	}
	return nil
}

func TestRelayMirrorsTheWindowAndOnlyServesTheMac(t *testing.T) {
	local, phone := loopbackPair(t)
	base := freeWindow(t, local.String(), phone)
	upstream(t, phone, base+3, base+windowSize+3)
	ctx, cancel := context.WithCancel(context.Background())
	r := newRelay(ctx, local, phone, slog.New(slog.DiscardHandler))
	defer func() { cancel(); r.close() }()

	r.slide(base)
	if got := len(r.ports()); got != windowSize+1 { // the window and the control port's slot
		t.Logf("bound %d ports (some may be taken)", got)
	}
	if err := echo(t, local, net.JoinHostPort(local.String(), strconv.Itoa(base+3))); err != nil {
		t.Fatalf("relayed connection: %v", err)
	}
	if err := echo(t, net.ParseIP("127.0.0.3"), net.JoinHostPort(local.String(), strconv.Itoa(base+3))); err == nil {
		t.Error("a connection from another address was relayed")
	}
	// The window moves on: the port behind it closes, the one ahead opens.
	r.slide(base + windowSize)
	if slices.Contains(r.ports(), base+3) {
		t.Error("the port behind the window is still bound")
	}
	if err := echo(t, local, net.JoinHostPort(local.String(), strconv.Itoa(base+windowSize+3))); err != nil {
		t.Fatalf("relayed connection after sliding: %v", err)
	}
}

func TestRelayHoldsWhenThePhoneRefuses(t *testing.T) {
	local, phone := loopbackPair(t)
	base := freeWindow(t, local.String(), phone)
	ctx, cancel := context.WithCancel(context.Background())
	r := newRelay(ctx, local, phone, slog.New(slog.DiscardHandler))
	r.slide(base)
	c, err := net.Dial("tcp", net.JoinHostPort(local.String(), strconv.Itoa(base)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !isTimeout(err) {
		t.Fatalf("the connection ended straight away (%v); remotepairingd would retry in a tight loop", err)
	}
	cancel()
	r.close() // returns although the hold had 10 s left
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// fakeRunner stands in for dns-sd, log and tailscale.
type fakeRunner struct {
	zone      string
	endpoints []string // lines `log stream` prints
	status    string   // tailscale status --json
	mu        sync.Mutex
	adverts   [][]string
}

func (f *fakeRunner) Run(ctx context.Context, c run.Cmd) (run.Result, error) {
	switch {
	case c.Name == "/usr/bin/dns-sd" && c.Args[0] == "-Z":
		_, _ = io.WriteString(c.Stdout, f.zone)
	case c.Name == "/usr/bin/dns-sd" && c.Args[0] == "-P":
		f.mu.Lock()
		f.adverts = append(f.adverts, c.Args)
		f.mu.Unlock()
	case c.Name == "/usr/bin/log":
		for _, l := range f.endpoints {
			_, _ = io.WriteString(c.Stdout, l+"\n")
		}
	case strings.HasSuffix(c.Name, "tailscale"):
		_, _ = io.WriteString(c.Stdout, f.status)
		return run.Result{}, nil
	}
	<-ctx.Done() // dns-sd and log run until they are stopped
	return run.Result{ExitCode: -1}, ctx.Err()
}

func loopbackIface(t *testing.T) string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 {
			return i.Name
		}
	}
	t.Skip("no loopback interface")
	return ""
}

func newManager(t *testing.T, f *fakeRunner) *Manager {
	return &Manager{
		DataDir: t.TempDir(), Runner: f, Log: slog.New(slog.DiscardHandler), LookFor: time.Second,
		Settings: config.BridgeSettings{Interface: loopbackIface(t)},
		ProbeFor: 2 * time.Second,
	}
}

func TestOnNeedsARememberedAdvert(t *testing.T) {
	m := newManager(t, &fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	if _, err := m.On(ctx, time.Hour, "127.0.0.2"); !errors.Is(err, ErrNoAdvert) {
		t.Fatalf("On = %v, want ErrNoAdvert", err)
	}
	if s := m.Status(); s.On || !strings.Contains(s.Detail, "hasn't seen the phone") {
		t.Errorf("status = %+v", s)
	}
}

func TestOnAdvertisesFollowsTunnelsAndEnds(t *testing.T) {
	local, phone := loopbackPair(t)
	base := freeWindow(t, local.String(), phone)
	if l, err := net.Listen("tcp", net.JoinHostPort(local.String(), strconv.Itoa(controlPort))); err != nil {
		t.Skipf("control port busy: %v", err)
	} else {
		_ = l.Close()
	}
	f := &fakeRunner{zone: zone, endpoints: []string{"x Got tunnel endpoint: '127.0.0.1%lo:" + strconv.Itoa(base) + "', includePeerToPeer: false"}}
	m := newManager(t, f)
	var probes atomic.Int32
	m.Probe = func(context.Context) error {
		if probes.Add(1) < 2 {
			return errors.New("tunnel refused")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, func() bool { _, ok := latest(m.load().Adverts); return ok })

	b, err := m.On(ctx, 48*time.Hour, phone)
	if err != nil {
		t.Fatal(err)
	}
	if !b.On || !b.Reachable || b.Phone != phone || time.Until(b.Until) > 12*time.Hour+time.Minute {
		t.Fatalf("status = %+v, want on, reachable, capped at 12h", b)
	}
	f.mu.Lock()
	args := f.adverts[0]
	f.mu.Unlock()
	if want := []string{"-P", "EC2FAA5B-EAFA-49E9-9432-2DE0198373E6", ServiceType, "local.", "49152", bridgeHost, local.String()}; !slices.Equal(args[:7], want) {
		t.Errorf("advert args = %q", args)
	}
	waitFor(t, func() bool { return m.load().Port == base })
	m.mu.Lock()
	ports := m.cur.relay.ports()
	m.mu.Unlock()
	if !slices.Contains(ports, base) || !slices.Contains(ports, base+windowSize-1) {
		t.Errorf("window not at the logged tunnel port %d: %v", base, ports)
	}
	if s := m.load(); s.Until.IsZero() || s.Phone != phone {
		t.Errorf("lease not saved: %+v", s)
	}

	if b := m.Off(); b.On {
		t.Errorf("after Off: %+v", b)
	}
	if s := m.load(); !s.Until.IsZero() || s.Port != base || len(s.Adverts) != 1 {
		t.Errorf("after Off the state keeps adverts and the port, not the lease: %+v", s)
	}
}

func TestAShorterLeaseEndsSooner(t *testing.T) {
	_, phone := loopbackPair(t)
	m := newManager(t, &fakeRunner{})
	m.update(func(s *state) { s.Adverts = ParseZone(zone, time.Now()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.base != nil })
	if _, err := m.On(ctx, time.Hour, phone); err != nil {
		t.Skipf("bridge didn't start: %v", err)
	}
	if _, err := m.On(ctx, 200*time.Millisecond, phone); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !m.Status().On })
	if !m.load().Until.IsZero() {
		t.Error("the ended lease is still saved")
	}
}

func TestResolvePhoneFromTheTailnet(t *testing.T) {
	status := `{"Peer": {
		"a": {"DNSName": "iphone183.tail.ts.net.", "OS": "iOS", "Online": true, "TailscaleIPs": ["fd7a::1", "100.86.244.6"]},
		"b": {"DNSName": "iphone-13-mini.tail.ts.net.", "OS": "iOS", "Online": false, "TailscaleIPs": ["100.110.29.112"]},
		"c": {"DNSName": "lattice.tail.ts.net.", "OS": "linux", "Online": true, "TailscaleIPs": ["100.111.134.21"]}}}`
	m := newManager(t, &fakeRunner{status: status})
	m.Tailscale = func() (string, error) { return "/bin/tailscale", nil }
	ctx := context.Background()
	for in, want := range map[string]string{"": "100.86.244.6", "iphone-13-mini": "100.110.29.112", "100.1.2.3": "100.1.2.3"} {
		got, err := m.resolvePhone(ctx, in)
		if err != nil || got != want {
			t.Errorf("resolvePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	m = newManager(t, &fakeRunner{status: strings.Replace(status, `"Online": false`, `"Online": true`, 1)})
	m.Tailscale = func() (string, error) { return "/bin/tailscale", nil }
	if _, err := m.resolvePhone(ctx, ""); err == nil || !strings.Contains(err.Error(), "several iPhones") {
		t.Errorf("two online iPhones: %v", err)
	}
}

func TestRunResumesASavedLease(t *testing.T) {
	_, phone := loopbackPair(t)
	f := &fakeRunner{}
	m := newManager(t, f)
	m.update(func(s *state) {
		s.Adverts = ParseZone(zone, time.Now())
		s.Until, s.Phone = time.Now().Add(time.Hour), phone
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, func() bool { return m.Status().On })
	cancel()
	<-done
	if m.Status().On {
		t.Error("the bridge outlived eve serve")
	}
	if m.load().Until.IsZero() {
		t.Error("stopping eve serve ended the lease; the next server should resume it")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
