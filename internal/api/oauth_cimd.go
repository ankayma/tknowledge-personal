package api

// Client ID Metadata Documents (draft-ietf-oauth-client-id-metadata-document):
// a client — Claude among them — names itself with an HTTPS URL, and this
// server fetches that URL to learn its redirect URIs. Fetching a URL a caller
// chose is a server-side request forgery risk, so the fetch is fenced: HTTPS
// on port 443 only, every resolved address must be public, no redirects, a
// short timeout, and a small size cap.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	cimdTimeout = 5 * time.Second
	cimdMaxSize = 64 << 10
	cimdCache   = 5 * time.Minute
)

type cimdDoc struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

type cimdEntry struct {
	doc cimdDoc
	at  time.Time
}

// blockedNets are ranges that are not ordinary public addresses beyond what
// net.IP's own predicates cover.
var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32",
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// publicIP reports whether ip is an ordinary public unicast address.
func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// guardDial runs after DNS resolution, on the address actually dialled, so a
// name that resolves to a private address is refused however it is spelled.
func guardDial(network, address string, _ syscall.RawConn) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if port != "443" {
		return fmt.Errorf("client metadata is fetched on port 443 only, not %s", port)
	}
	if !publicIP(net.ParseIP(host)) {
		return fmt.Errorf("client metadata host resolves to a non-public address %s", host)
	}
	return nil
}

// guardedClient is the default fetcher for client metadata documents.
func guardedClient() *http.Client {
	dialer := &net.Dialer{Timeout: cimdTimeout, Control: guardDial}
	return &http.Client{
		Timeout: cimdTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    cimdTimeout,
			ResponseHeaderTimeout:  cimdTimeout,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("client metadata documents are not followed through redirects")
		},
	}
}

func (o *OAuth) fetcher() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return guardedClient()
}

// isCIMD reports whether a client_id is a metadata document URL.
func isCIMD(clientID string) bool { return strings.HasPrefix(clientID, "https://") }

// fetchCIMD reads and checks one client metadata document.
func (o *OAuth) fetchCIMD(ctx context.Context, clientID string) (cimdDoc, error) {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return cimdDoc{}, errors.New("client_id must be an https URL with a host and no credentials or fragment")
	}
	o.mu.Lock()
	if e, ok := o.cimd[clientID]; ok && o.now().Sub(e.at) < cimdCache {
		o.mu.Unlock()
		return e.doc, nil
	}
	o.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return cimdDoc{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.fetcher().Do(req)
	if err != nil {
		return cimdDoc{}, fmt.Errorf("fetch client metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cimdDoc{}, fmt.Errorf("client metadata answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxSize+1))
	if err != nil {
		return cimdDoc{}, err
	}
	if len(body) > cimdMaxSize {
		return cimdDoc{}, errors.New("client metadata document is too large")
	}
	var doc cimdDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return cimdDoc{}, fmt.Errorf("client metadata is not JSON: %w", err)
	}
	if doc.ClientID != clientID {
		return cimdDoc{}, errors.New("client metadata names a different client_id than the URL it was fetched from")
	}
	if len(doc.RedirectURIs) == 0 {
		return cimdDoc{}, errors.New("client metadata lists no redirect_uris")
	}
	o.mu.Lock()
	if o.cimd == nil {
		o.cimd = map[string]cimdEntry{}
	}
	o.cimd[clientID] = cimdEntry{doc: doc, at: o.now()}
	o.mu.Unlock()
	return doc, nil
}

// --- redirect URIs ------------------------------------------------------------

func isLoopbackHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// redirectMatches checks a requested redirect against the registered ones:
// exactly, or — for a loopback redirect, which a native client such as Claude
// Code binds on a port that changes every run — with the port ignored
// (RFC 8252 §7.3, applied to localhost as well because Claude Code declares it).
func redirectMatches(registered []string, requested string) bool {
	for _, r := range registered {
		if r == requested {
			return true
		}
	}
	rq, err := url.Parse(requested)
	if err != nil || rq.Scheme != "http" || !isLoopbackHost(rq.Hostname()) || rq.Fragment != "" {
		return false
	}
	for _, r := range registered {
		ru, err := url.Parse(r)
		if err != nil || ru.Scheme != "http" {
			continue
		}
		if strings.EqualFold(ru.Hostname(), rq.Hostname()) && ru.Path == rq.Path && ru.RawQuery == rq.RawQuery {
			return true
		}
	}
	return false
}

// checkRegisteredRedirect is what a dynamically registered client may name:
// an https URL, or a loopback http URL for a native client.
func checkRegisteredRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return fmt.Errorf("%q is not an absolute redirect URI without a fragment", raw)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("%q: redirect URIs are https, or http on a loopback address", raw)
}

// decodeJSONBody reads a small JSON body.
func decodeJSONBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("body is not valid JSON: %w", err)
	}
	return nil
}
