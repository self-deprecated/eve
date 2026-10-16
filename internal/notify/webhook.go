package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/self-deprecated/eve/internal/config"
)

// Secrets resolves the HMAC secret for a target; "" means send unsigned.
type Secrets interface {
	Secret(t Target) (string, error)
}

// ConfigSecrets resolves secrets from server.yml's notify section: the human hook's secret for
// the human target, and for webhooks the HookToken with the longest URL prefix match. End
// prefixes with "/" so "https://hooks.example/" cannot match "https://hooks.example.evil/".
type ConfigSecrets struct {
	Notify config.Notify
	// Resolve reads a secret reference; nil means config.ResolveSecret ("file:" or "env:").
	Resolve func(ref string) (string, error)
}

// Secret implements Secrets.
func (c ConfigSecrets) Secret(t Target) (string, error) {
	var ref string
	switch t.Kind {
	case KindHuman:
		if h := c.Notify.Human; h != nil && h.URL == t.Address {
			ref = h.Secret
		}
	case KindWebhook:
		best := -1
		for _, ht := range c.Notify.Secrets {
			if ht.Prefix != "" && strings.HasPrefix(t.Address, ht.Prefix) && len(ht.Prefix) > best {
				best, ref = len(ht.Prefix), ht.Secret
			}
		}
	}
	if ref == "" {
		return "", nil
	}
	resolve := c.Resolve
	if resolve == nil {
		resolve = config.ResolveSecret
	}
	s, err := resolve(ref)
	if err != nil {
		return "", fmt.Errorf("webhook secret: %w", err)
	}
	return s, nil
}

// Sign returns the X-Eve-Signature value for body: "sha256=" and the hex HMAC-SHA256.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// DeliveryHeader returns the X-Eve-Delivery value: the notification's deliveryId, or the
// outbox row id if the store left it empty. Either is stable across retries.
func DeliveryHeader(row Delivery) string {
	if row.Payload.DeliveryID != "" {
		return row.Payload.DeliveryID
	}
	return strconv.FormatInt(row.ID, 10)
}

// noRedirect makes a client return 3xx responses instead of following them: a redirect could
// lead past the allow-list.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// postWebhook POSTs v as JSON to the row's target.
func (d *Deliverer) postWebhook(ctx context.Context, row Delivery, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: encode body: %w", ErrPermanent, err)
	}
	var secret string
	if d.Secrets != nil {
		if secret, err = d.Secrets.Secret(row.Target); err != nil {
			return err // retryable: a missing secret file can be fixed within MaxAge
		}
	}
	ctx, cancel := context.WithTimeout(ctx, WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, row.Target.Address, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	version := d.Version
	if version == "" {
		version = "dev"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eve/"+version)
	req.Header.Set("X-Eve-Event", row.Event)
	req.Header.Set("X-Eve-Delivery", DeliveryHeader(row))
	if secret != "" {
		req.Header.Set("X-Eve-Signature", Sign(secret, body))
	}

	hc := http.Client{CheckRedirect: noRedirect}
	if d.HTTP != nil {
		hc = *d.HTTP
		hc.CheckRedirect = noRedirect
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", redactURL(row.Target.Address), err)
	}
	defer func() { _ = resp.Body.Close() }()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	code := resp.StatusCode
	switch {
	case code/100 == 2:
		return nil
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return fmt.Errorf("POST %s: HTTP %d", redactURL(row.Target.Address), code)
	}
	return fmt.Errorf("%w: POST %s: HTTP %d %s", ErrPermanent, redactURL(row.Target.Address), code,
		strings.TrimSpace(string(snippet)))
}

// redactURL drops the query, which often carries a webhook token, from logged URLs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	if u.RawQuery != "" {
		u.RawQuery = "…"
	}
	return u.String()
}

// AllowedURL checks a notify URL against notify.allowHosts at submit time. The URL must be https,
// or http to a loopback or tailnet address (100.64.0.0/10, fd7a:115c:a1e0::/48, *.ts.net), must
// carry no userinfo, and its host must match one of allowHosts. Patterns are path.Match globs
// such as "*.tail6e682f.ts.net", compared case-insensitively against the hostname, or against
// host:port when the pattern names a port.
func AllowedURL(rawURL string, allowHosts []string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("notify URL: %w", err)
	}
	if u.User != nil {
		return errors.New("notify URL must not contain credentials; configure a secret in notify.secrets")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || u.Opaque != "" {
		return fmt.Errorf("notify URL %q has no host", redactURL(rawURL))
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !privateHost(host) {
			return fmt.Errorf("notify URL %s: plain http is only allowed to loopback or tailnet hosts", redactURL(rawURL))
		}
	default:
		return fmt.Errorf("notify URL %s: scheme must be https (or http on the tailnet)", redactURL(rawURL))
	}
	if len(allowHosts) == 0 {
		return errors.New("notify URLs are disabled: server.yml has no notify.allowHosts")
	}
	hostPort := strings.ToLower(u.Host)
	for _, p := range allowHosts {
		p = strings.ToLower(strings.TrimSpace(p))
		if ok, _ := path.Match(p, host); ok {
			return nil
		}
		if ok, _ := path.Match(p, hostPort); ok {
			return nil
		}
	}
	return fmt.Errorf("notify URL host %s is not in notify.allowHosts", host)
}

var (
	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// privateHost reports whether host is loopback or on the tailnet, where plain http is
// acceptable because WireGuard (or the kernel) already protects the hop.
func privateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || tailnetV4.Contains(ip) || tailnetV6.Contains(ip)
}
