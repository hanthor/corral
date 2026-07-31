// Package proxmoxbe drives a real Proxmox VE cluster as a Corral backend
// (ADR-0009).
//
// The name is deliberately not `proxmox`: that package is the *compat server*,
// which answers PVE-shaped questions about Corral's own fleet. This package asks
// PVE-shaped questions of somebody else's cluster. Confusing the two would be
// easy and expensive, so they do not share a name and do not share types.
//
// It is the first backend that is an HTTP client rather than a command runner.
// Every other backend shells out because that CLI *is* the supported interface
// (kubectl, virtctl, incus, virsh); Proxmox's supported interface is the REST
// API, and `pvesh` only exists on the node itself. So there is no shell.Runner
// seam here — the seam is the http.RoundTripper, and tests use httptest.
package proxmoxbe

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config is one PVE cluster endpoint, as a Corral context carries it.
type Config struct {
	// Host is the API endpoint: "pve.example.com", "pve.example.com:8006", or a
	// full "https://…" URL. The port defaults to 8006.
	Host string
	// Token is a PVE API token in its full form,
	// "USER@REALM!TOKENID=UUID". Tokens are revocable and do not expire like
	// tickets, which is why they are the only auth this backend accepts.
	Token string
	// Fingerprint pins the server certificate (SHA-256, hex, colons optional).
	// Self-signed certificates are the norm on PVE, and pinning is how trust is
	// established without disabling verification.
	Fingerprint string
	// Insecure skips verification entirely. Explicit, per-context, and never a
	// default — a silent skip is how a homelab convenience becomes a habit.
	Insecure bool
	// Node is the default node for operations that need one before the
	// inventory has resolved (creates, mostly). Empty means "ask the cluster".
	Node string
}

// Client talks to one cluster.
type Client struct {
	cfg  Config
	base string
	http *http.Client
}

// Timeout bounds every request. A PVE that is up but wedged should fail a list,
// not hang a whole fleet aggregation — the same reasoning behind the kubectl
// request timeout in pkg/shell.
const Timeout = 20 * time.Second

// New builds a client. It fails on a configuration that cannot work rather than
// at the first request, so `corral context add` can reject it immediately.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, fmt.Errorf("proxmox: host is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("proxmox: an API token is required (USER@REALM!TOKENID=UUID); " +
			"create one with: pveum user token add <user>@pve corral --privsep 0")
	}
	if !strings.Contains(cfg.Token, "!") || !strings.Contains(cfg.Token, "=") {
		return nil, fmt.Errorf("proxmox: token %q is not in USER@REALM!TOKENID=UUID form", redactToken(cfg.Token))
	}

	base := cfg.Host
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("proxmox: host %q: %w", cfg.Host, err)
	}
	if parsed.Port() == "" {
		parsed.Host += ":8006"
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/api2/json"

	transport, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:  cfg,
		base: parsed.String(),
		http: &http.Client{Timeout: Timeout, Transport: transport},
	}, nil
}

// WithHTTPClient replaces the transport. Tests point this at an httptest server;
// nothing in production does.
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	c.http = h
	return c
}

// Node is the configured default node, or "" when the inventory should decide.
func (c *Client) Node() string { return c.cfg.Node }

func transportFor(cfg Config) (http.RoundTripper, error) {
	switch {
	case cfg.Fingerprint != "":
		want := normaliseFingerprint(cfg.Fingerprint)
		if len(want) != 64 {
			return nil, fmt.Errorf("proxmox: fingerprint must be a SHA-256 hex digest, got %d hex chars", len(want))
		}
		// Verification is done by hand against the pin, which is what allows a
		// self-signed certificate to be trusted *specifically* rather than
		// trusting anything a MITM presents.
		return &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // replaced by the pin below
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				for _, raw := range rawCerts {
					sum := sha256.Sum256(raw)
					if hex.EncodeToString(sum[:]) == want {
						return nil
					}
				}
				return fmt.Errorf("proxmox: server certificate does not match the pinned fingerprint")
			},
		}}, nil
	case cfg.Insecure:
		return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, nil //nolint:gosec // explicit per-context opt-in
	default:
		return http.DefaultTransport, nil
	}
}

func normaliseFingerprint(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(s)))
}

// redactToken keeps the identity and drops the secret, so an error message is
// actionable without leaking the token into a log.
func redactToken(token string) string {
	if i := strings.Index(token, "="); i > 0 {
		return token[:i+1] + "…"
	}
	return "…"
}

// APIError is a PVE error response. It carries the status so callers can tell a
// refusal (400: retrying cannot help) from a failure (500: it might), the same
// distinction pkg/snapshot draws between Unsupported and a plain error.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("proxmox: %s %s: %s (HTTP %d)", e.Method, e.Path, msg, e.Status)
}

// Unauthorized reports whether the token was rejected, which the doctor reports
// differently from an unreachable cluster.
func (e *APIError) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// get unmarshals PVE's `{"data": …}` envelope into out.
func (c *Client) get(path string, out any) error {
	return c.do(http.MethodGet, path, nil, out)
}

// post sends form-encoded parameters, which is what the PVE API expects, and
// returns the raw data field — a UPID string for anything asynchronous.
func (c *Client) post(path string, params url.Values) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(http.MethodPost, path, params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) put(path string, params url.Values) error {
	return c.do(http.MethodPut, path, params, nil)
}

func (c *Client) delete(path string, params url.Values) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(http.MethodDelete, path, params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) do(method, path string, params url.Values, out any) error {
	var body io.Reader
	if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.Token)
	if params != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("proxmox: reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Method: method, Path: path,
			Message: errorMessage(payload)}
	}
	if out == nil {
		return nil
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("proxmox: %s %s returned unparseable JSON: %w", method, path, err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("proxmox: %s %s: decoding data: %w", method, path, err)
	}
	return nil
}

// errorMessage pulls the human part out of a PVE error body. PVE reports
// per-parameter validation errors in `errors`, and those are the useful ones —
// "cores: value must be an integer" beats "400 Bad Request".
func errorMessage(payload []byte) string {
	var body struct {
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		text := strings.TrimSpace(string(payload))
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		return text
	}
	var parts []string
	for field, problem := range body.Errors {
		parts = append(parts, field+": "+problem)
	}
	msg := strings.TrimSpace(body.Message)
	if len(parts) > 0 {
		if msg != "" {
			msg += " — "
		}
		msg += strings.Join(parts, "; ")
	}
	return msg
}

// unquote turns a PVE data field that is a bare JSON string (a UPID) into that
// string. Anything else comes back empty, which is how a synchronous endpoint's
// response is distinguished from a task id.
func unquote(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Version is the cluster's reported version, used by the doctor to prove both
// reachability and that the token is accepted in one call.
func (c *Client) Version() (string, error) {
	var v struct {
		Version string `json:"version"`
		Release string `json:"release"`
	}
	if err := c.get("/version", &v); err != nil {
		return "", err
	}
	if v.Release != "" && v.Release != v.Version {
		return v.Version + " (" + v.Release + ")", nil
	}
	return v.Version, nil
}

// dump is a debugging aid kept out of the exported surface.
func dump(v any) string {
	out, _ := json.Marshal(v)
	return string(bytes.TrimSpace(out))
}
