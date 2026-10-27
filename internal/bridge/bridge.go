// Package bridge reaches the paired iPhone over Tailscale while it is on another Wi-Fi network
// (docs/design.md §6.4.1). CoreDevice only finds a phone through its Bonjour advert on the build Mac's
// network and only connects on the interface it heard that advert on. So the bridge republishes the
// phone's remembered advert pointing at the Mac's own LAN address, and relays what remotepairingd
// then connects to there to the same ports on the phone's tailnet address: the control channel
// (49152) and each tunnel, whose port the phone takes from a counter that goes up by one per
// tunnel. The relay keeps a small window of ports ahead of the last tunnel port remotepairingd
// logged. Everything it carries is CoreDevice's own end-to-end encrypted traffic, and it accepts
// connections only from the Mac itself.
//
// The bridge runs inside `eve serve` for a lease (`eve bridge on --for 2h`), never by itself.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/run"
)

// rememberEvery is how often an idle server looks for the phone's advert on its network.
const rememberEvery = 15 * time.Minute

// Manager owns the bridge for `eve serve`: at most one lease at a time.
type Manager struct {
	DataDir  string
	Settings config.BridgeSettings
	Runner   run.Runner
	Log      *slog.Logger
	// Probe makes one device call that needs a tunnel (devicectl device info lockState); nil
	// skips the reachability check.
	Probe func(ctx context.Context) error
	// Tailscale finds the tailscale CLI, for picking the phone when none is configured.
	Tailscale func() (string, error)
	// ProbeFor bounds how long On waits for the phone to answer through the bridge.
	ProbeFor time.Duration
	// LookFor is how long a capture listens for adverts.
	LookFor time.Duration
	Now     func() time.Time

	mu   sync.Mutex
	base context.Context
	cur  *session
}

type session struct {
	cancel context.CancelFunc
	done   chan struct{}
	// moved wakes the lease timer when On moves until.
	moved  chan struct{}
	until  time.Time
	phone  string
	advert Advert
	relay  *relay
	// err is why the session stopped early (dns-sd or log stream died), "" while it runs.
	err       string
	reachable bool
}

// state is bridge.json in the data directory.
type state struct {
	Adverts []Advert  `json:"adverts,omitempty"`
	Until   time.Time `json:"until,omitzero"`
	Phone   string    `json:"phone,omitempty"`
	// Port is the last tunnel port seen, where the next session's window starts.
	Port int `json:"port,omitempty"`
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) path() string { return filepath.Join(m.DataDir, "bridge.json") }

func (m *Manager) load() state {
	var s state
	if b, err := os.ReadFile(m.path()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (m *Manager) save(s state) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path())
}

// update applies f to the saved state; m.mu is held.
func (m *Manager) update(f func(*state)) {
	s := m.load()
	f(&s)
	if err := m.save(s); err != nil {
		m.Log.Warn("bridge: saving state", "err", err)
	}
}

// Run resumes a lease that outlived the last server, then remembers the phone's advert every
// rememberEvery while no lease is active. It returns when ctx ends, stopping the bridge but
// keeping its lease for the next server.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.base = ctx
	s := m.load()
	var resumed *session
	if s.Until.After(m.now()) {
		if err := m.start(s.Until, s.Phone); err != nil {
			m.Log.Warn("bridge: resuming the lease", "err", err)
			m.update(func(s *state) { s.Until, s.Phone = time.Time{}, "" })
		}
		resumed = m.cur
	}
	m.mu.Unlock()
	if resumed != nil {
		go m.probe(ctx, resumed)
	}
	t := time.NewTicker(rememberEvery)
	defer t.Stop()
	for {
		if !m.active() {
			if _, err := m.Remember(ctx); err != nil && ctx.Err() == nil {
				m.Log.Debug("bridge: looking for the phone's advert", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if m.cur != nil {
				m.stop()
			}
			m.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

func (m *Manager) active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && m.cur.err == ""
}

// Remember looks for phone adverts on the build Mac's network and saves any it finds. While a
// bridge runs its own advert is skipped.
func (m *Manager) Remember(ctx context.Context) ([]Advert, error) {
	look := m.LookFor
	if look == 0 {
		look = 3 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, look)
	defer cancel()
	var out bytes.Buffer
	_, err := m.Runner.Run(cctx, run.Cmd{Name: "/usr/bin/dns-sd", Args: []string{"-Z", ServiceType, "local."}, Env: []string{"PATH=/usr/bin:/bin"}, Stdout: &out})
	if err != nil && cctx.Err() == nil {
		return nil, fmt.Errorf("dns-sd: %w", err)
	}
	found := ParseZone(out.String(), m.now())
	if len(found) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.update(func(s *state) {
		for _, a := range found {
			s.Adverts = slicesDeleteName(s.Adverts, a.Name)
			s.Adverts = append(s.Adverts, a)
		}
	})
	return found, nil
}

func slicesDeleteName(as []Advert, name string) []Advert {
	out := as[:0]
	for _, a := range as {
		if a.Name != name {
			out = append(out, a)
		}
	}
	return out
}

// latest returns the advert seen most recently.
func latest(as []Advert) (Advert, bool) {
	if len(as) == 0 {
		return Advert{}, false
	}
	sort.SliceStable(as, func(i, j int) bool { return as[i].SeenAt.After(as[j].SeenAt) })
	return as[0], true
}

// ErrNoAdvert means the phone has never been seen on the build Mac's network.
var ErrNoAdvert = errors.New("eve hasn't seen the phone on the build Mac's network yet, so it can't stand in for it: " +
	"put the phone on the Mac's Wi-Fi once (eve remembers it within 15 minutes, or at the next `eve bridge on`), then try again")

// On starts the bridge, or extends the running one, until now+d (capped by maxDuration). It
// then waits up to ProbeFor for a device call to go through and reports whether one did.
func (m *Manager) On(ctx context.Context, d time.Duration, phone string) (api.Bridge, error) {
	if d <= 0 {
		return api.Bridge{}, errors.New("the bridge needs a positive duration")
	}
	if max := m.Settings.MaxLease(); d > max {
		d = max
	}
	// Refresh the advert first; the phone may well be on the Mac's network right now.
	if _, err := m.Remember(ctx); err != nil {
		m.Log.Debug("bridge: looking for the phone's advert", "err", err)
	}
	until := m.now().Add(d)
	m.mu.Lock()
	if m.cur != nil && m.cur.err == "" && (phone == "" || phone == m.cur.phone) {
		m.cur.until = until
		select {
		case m.cur.moved <- struct{}{}:
		default:
		}
		m.update(func(s *state) { s.Until = until })
	} else if err := m.start(until, phone); err != nil {
		m.mu.Unlock()
		return api.Bridge{}, err
	} else {
		m.update(func(s *state) { s.Until, s.Phone = until, m.cur.phone })
	}
	cur := m.cur
	m.mu.Unlock()
	m.probe(ctx, cur)
	return m.Status(), nil
}

// start begins a session; m.mu is held. phone "" uses the configured or the only online iPhone.
func (m *Manager) start(until time.Time, phone string) error {
	if m.base == nil {
		return errors.New("the bridge only runs inside eve serve")
	}
	s := m.load()
	ad, ok := latest(s.Adverts)
	if !ok {
		return ErrNoAdvert
	}
	addr, err := m.resolvePhone(m.base, phone)
	if err != nil {
		return err
	}
	local, err := lanAddress(m.Settings.Iface())
	if err != nil {
		return err
	}
	if m.cur != nil {
		m.stop()
	}
	ctx, cancel := context.WithCancel(m.base)
	r := newRelay(ctx, local, addr, m.Log)
	if err := r.listen(); err != nil {
		cancel()
		return fmt.Errorf("the bridge can't listen on %s:%d: %w", local, controlPort, err)
	}
	if s.Port != 0 {
		r.slide(s.Port + 1)
	}
	cur := &session{cancel: cancel, done: make(chan struct{}), moved: make(chan struct{}, 1), until: until, phone: addr, advert: ad, relay: r}
	m.cur = cur
	m.Log.Info("bridge on", "phone", addr, "local", local.String(), "until", until.Format(time.RFC3339))
	var wg sync.WaitGroup
	fail := func(what string, err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if ctx.Err() == nil && cur.err == "" {
			cur.err = fmt.Sprintf("%s stopped: %v", what, err)
			m.Log.Warn("bridge: " + cur.err)
			cancel()
		}
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		args := append([]string{"-P", ad.Name, ServiceType, "local.", strconv.Itoa(controlPort), bridgeHost, local.String()}, ad.TXT...)
		_, err := m.Runner.Run(ctx, run.Cmd{Name: "/usr/bin/dns-sd", Args: args, Env: []string{"PATH=/usr/bin:/bin"}})
		fail("advertising the phone (dns-sd)", errOr(err, "exited"))
	}()
	go func() {
		defer wg.Done()
		w := &lineWriter{line: func(l string) {
			if ip, port, ok := tunnelEndpoint(l); ok && ip.Equal(local) {
				r.slide(port)
				m.mu.Lock()
				m.update(func(s *state) { s.Port = port })
				m.mu.Unlock()
			}
		}}
		_, err := m.Runner.Run(ctx, run.Cmd{
			Name: "/usr/bin/log", Env: []string{"PATH=/usr/bin:/bin"}, Stdout: w,
			Args: []string{"stream", "--style", "compact", "--predicate", `process == "remotepairingd" AND eventMessage CONTAINS "Got tunnel endpoint"`},
		})
		fail("following remotepairingd's log", errOr(err, "exited"))
	}()
	go func() {
		defer wg.Done()
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-cur.moved:
			case <-t.C:
			}
			m.mu.Lock()
			if left := cur.until.Sub(m.now()); left > 0 {
				t.Reset(left)
				m.mu.Unlock()
				continue
			}
			m.Log.Info("bridge lease ended")
			if m.cur == cur {
				m.cur = nil
				m.update(func(s *state) { s.Until, s.Phone = time.Time{}, "" })
			}
			m.mu.Unlock()
			cancel()
			return
		}
	}()
	go func() {
		wg.Wait()
		r.close()
		close(cur.done)
	}()
	return nil
}

func errOr(err error, s string) error {
	if err != nil {
		return err
	}
	return errors.New(s)
}

// probe retries a device call until one goes through or ProbeFor passes. The first attempts
// fail while remotepairingd discovers the advert, or when the phone's tunnel port is outside
// the window: remotepairingd logs it, the window moves, and the next attempt gets through.
func (m *Manager) probe(ctx context.Context, cur *session) {
	if m.Probe == nil {
		return
	}
	limit := m.ProbeFor
	if limit == 0 {
		limit = 40 * time.Second
	}
	deadline := m.now().Add(limit)
	for {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := m.Probe(pctx)
		cancel()
		m.mu.Lock()
		if err == nil {
			cur.reachable = true
		}
		stopped := cur.err != "" || m.cur != cur
		m.mu.Unlock()
		if err == nil || stopped || ctx.Err() != nil || m.now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// Off stops the bridge, if it runs.
func (m *Manager) Off() api.Bridge {
	m.mu.Lock()
	if m.cur != nil {
		m.stop()
		m.update(func(s *state) { s.Until, s.Phone = time.Time{}, "" })
		m.Log.Info("bridge off")
	}
	m.mu.Unlock()
	return m.Status()
}

// stop ends the current session and waits for it; m.mu is held.
func (m *Manager) stop() {
	cur := m.cur
	m.cur = nil
	cur.cancel()
	m.mu.Unlock()
	<-cur.done
	m.mu.Lock()
}

// Status describes the bridge.
func (m *Manager) Status() api.Bridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := api.Bridge{MaxDuration: m.Settings.MaxLease().String()}
	if ad, ok := latest(m.load().Adverts); ok {
		b.AdvertSeenAt = ad.SeenAt
	}
	cur := m.cur
	switch {
	case cur == nil && b.AdvertSeenAt.IsZero():
		b.Detail = ErrNoAdvert.Error()
	case cur == nil:
		b.Detail = "off: the build Mac only reaches the phone on its own network"
	case cur.err != "":
		b.Detail = "the bridge stopped: " + cur.err + "; run `eve bridge on` again"
	default:
		b.On, b.Until, b.Phone, b.Reachable = true, cur.until, cur.phone, cur.reachable
		if !cur.reachable {
			b.Detail = "the bridge is on, but no device call has gone through it yet: the phone needs Wi-Fi (any network) " +
				"with Tailscale connected, and to be awake; `eve bridge on` again checks once more"
		}
	}
	return b
}

var endpointRe = regexp.MustCompile(`Got tunnel endpoint: '([^'%]+)(?:%[^':]*)?:(\d+)'`)

// tunnelEndpoint parses remotepairingd's "Got tunnel endpoint: '192.168.0.103%en0:60822'" line.
func tunnelEndpoint(line string) (net.IP, int, bool) {
	m := endpointRe.FindStringSubmatch(line)
	if m == nil {
		return nil, 0, false
	}
	ip := net.ParseIP(strings.Trim(m[1], "[]"))
	port, err := strconv.Atoi(m[2])
	if ip == nil || err != nil || port <= controlPort || port > lastPort {
		return nil, 0, false
	}
	return ip, port, true
}

// lineWriter calls line for each complete line written to it.
type lineWriter struct {
	buf  []byte
	line func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 {
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// lanAddress returns the interface's IPv4 address: the one remotepairingd browses from.
func lanAddress(name string) (net.IP, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("the bridge needs the build Mac's network interface %s (device.bridge.interface): %w", name, err)
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.To4(), nil
		}
	}
	return nil, fmt.Errorf("%s has no IPv4 address: the build Mac must be on its network for the bridge", name)
}

// tailscalePeer is the part of `tailscale status --json` the bridge reads.
type tailscalePeer struct {
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	Online       bool     `json:"Online"`
	TailscaleIPs []string `json:"TailscaleIPs"`
}

// resolvePhone turns the phone's name or address into its tailnet IPv4 address. With none given
// (here or in device.bridge.phone) it picks the only iPhone online on the tailnet.
func (m *Manager) resolvePhone(ctx context.Context, phone string) (string, error) {
	if phone == "" {
		phone = m.Settings.Phone
	}
	if ip := net.ParseIP(phone); ip != nil {
		return ip.String(), nil
	}
	peers, tsErr := m.peers(ctx)
	if phone == "" {
		if tsErr != nil {
			return "", fmt.Errorf("set device.bridge.phone or pass --phone (the phone's tailnet name or address); finding it on the tailnet failed: %w", tsErr)
		}
		var phones []tailscalePeer
		var names []string
		for _, p := range peers {
			if p.OS == "iOS" && p.Online && len(p.TailscaleIPs) > 0 {
				phones = append(phones, p)
				names = append(names, firstLabel(p.DNSName))
			}
		}
		switch len(phones) {
		case 1:
			return ipv4(phones[0].TailscaleIPs), nil
		case 0:
			return "", errors.New("no iPhone is online on the tailnet: open Tailscale on the phone and connect it (or pass --phone)")
		}
		return "", fmt.Errorf("several iPhones are online on the tailnet (%s): pass --phone or set device.bridge.phone", strings.Join(names, ", "))
	}
	for _, p := range peers {
		if strings.EqualFold(firstLabel(p.DNSName), firstLabel(phone)) && len(p.TailscaleIPs) > 0 {
			return ipv4(p.TailscaleIPs), nil
		}
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, phone)
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("can't find the phone %q on the tailnet: %v", phone, errors.Join(tsErr, err))
	}
	return addrs[0], nil
}

func (m *Manager) peers(ctx context.Context) ([]tailscalePeer, error) {
	if m.Tailscale == nil {
		return nil, errors.New("no tailscale CLI")
	}
	bin, err := m.Tailscale()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := m.Runner.Run(cctx, run.Cmd{Name: bin, Args: []string{"status", "--json"}, Env: []string{"PATH=/usr/bin:/bin"}, Stdout: &out})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("tailscale status exited %d", res.ExitCode)
	}
	var st struct {
		Peer map[string]tailscalePeer `json:"Peer"`
	}
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	peers := make([]tailscalePeer, 0, len(st.Peer))
	for _, p := range st.Peer {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].DNSName < peers[j].DNSName })
	return peers, nil
}

func firstLabel(name string) string {
	name, _, _ = strings.Cut(name, ".")
	return name
}

func ipv4(ips []string) string {
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			return s
		}
	}
	return ips[0]
}
