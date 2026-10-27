package bridge

import (
	"strconv"
	"strings"
	"time"
)

// ServiceType is the Bonjour service a paired iPhone advertises for CoreDevice.
const ServiceType = "_remotepairing._tcp"

// bridgeHost is the host name the bridge's own advert uses, so a capture never remembers it.
const bridgeHost = "eve-bridge.local."

// Advert is a phone's Bonjour advert as the build Mac saw it on its own network. Its TXT record
// (identifier, authTag, protocol versions) is what lets remotepairingd recognise the paired phone
// before it connects, and it stays valid for days, so the bridge can republish it while the phone
// is away.
type Advert struct {
	Name   string    `json:"name"` // the Bonjour instance name
	TXT    []string  `json:"txt"`  // key=value strings, in the order advertised
	SeenAt time.Time `json:"seenAt"`
}

// ParseZone reads the output of `dns-sd -Z _remotepairing._tcp local.` and returns every advert
// with a TXT record, except the bridge's own. dns-sd repeats records as they are refreshed; the
// last copy of each wins.
func ParseZone(out string, seen time.Time) []Advert {
	suffix := "." + ServiceType
	byName := map[string]*Advert{}
	var order []string
	ours := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasSuffix(f[0], suffix) || strings.HasPrefix(line, ";") {
			continue
		}
		name := unescape(strings.TrimSuffix(f[0], suffix))
		switch f[1] {
		case "SRV": // priority weight port target
			if len(f) >= 6 && f[5] == bridgeHost {
				ours[name] = true
			}
		case "TXT":
			txt := quoted(line[strings.Index(line, "TXT")+3:])
			if len(txt) == 0 {
				continue
			}
			if byName[name] == nil {
				order = append(order, name)
			}
			byName[name] = &Advert{Name: name, TXT: txt, SeenAt: seen}
		}
	}
	var adverts []Advert
	for _, name := range order {
		if !ours[name] {
			adverts = append(adverts, *byName[name])
		}
	}
	return adverts
}

// quoted returns the double-quoted strings in s, with dns-sd's backslash escapes undone.
func quoted(s string) []string {
	var out []string
	for {
		start := strings.IndexByte(s, '"')
		if start < 0 {
			return out
		}
		var b strings.Builder
		i := start + 1
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		out = append(out, b.String())
		if i >= len(s) {
			return out
		}
		s = s[i+1:]
	}
}

// unescape undoes the \DDD and \c escapes dns-sd uses in instance names.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		if i+3 < len(s) {
			if n, err := strconv.Atoi(s[i+1 : i+4]); err == nil && n < 256 {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		i++
		b.WriteByte(s[i])
	}
	return b.String()
}
