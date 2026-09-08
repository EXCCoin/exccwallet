package vsp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccwallet/v2/internal/vspidentity"
)

const (
	vspRequestTimeout   = 15 * time.Second
	maxVSPResponseBytes = 4 << 20
)

// CanonicalVSPHost validates and normalizes a provider's HTTPS base URL.
func CanonicalVSPHost(host string) (string, error) {
	return vspidentity.CanonicalHost(host)
}

// ParseVSPIdentity validates a provider URL and canonical Ed25519 public key.
func ParseVSPIdentity(host, encodedKey string) (string, []byte, error) {
	host, err := CanonicalVSPHost(host)
	if err != nil {
		return "", nil, err
	}
	key, err := base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil || len(key) != ed25519.PublicKeySize ||
		base64.StdEncoding.EncodeToString(key) != encodedKey {
		return "", nil, fmt.Errorf("VSP public key must be a canonical base64 Ed25519 public key")
	}
	return host, key, nil
}

func vspRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) > 3 {
		return fmt.Errorf("too many VSP redirects")
	}
	first := via[0].URL
	next := req.URL
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			n, _ := strconv.Atoi(p)
			return strconv.Itoa(n)
		}
		return "443"
	}
	if next.Scheme != "https" || next.User != nil || next.Fragment != "" ||
		!strings.EqualFold(next.Hostname(), first.Hostname()) || port(next) != port(first) {
		return fmt.Errorf("VSP redirect must stay on the same HTTPS provider")
	}
	return nil
}

type client struct {
	http.Client
	url  string
	pub  []byte
	sign func(context.Context, string, stdaddr.Address) ([]byte, error)
}

type signer interface {
	SignMessage(ctx context.Context, message string, address stdaddr.Address) ([]byte, error)
}

func newClient(url string, pub []byte, s signer) *client {
	return &client{
		Client: http.Client{Timeout: vspRequestTimeout, CheckRedirect: vspRedirect},
		url:    url, pub: pub, sign: s.SignMessage,
	}
}

type BadRequestError struct {
	HTTPStatus int    `json:"-"`
	Code       int    `json:"code"`
	Message    string `json:"message"`
}

func (e *BadRequestError) Error() string { return e.Message }

func (c *client) post(ctx context.Context, path string, addr stdaddr.Address, resp, req interface{}) error {
	return c.do(ctx, "POST", path, addr, resp, req)
}

func (c *client) get(ctx context.Context, path string, resp interface{}) error {
	return c.do(ctx, "GET", path, nil, resp, nil)
}

func (c *client) do(ctx context.Context, method, path string, addr stdaddr.Address, resp, req interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, vspRequestTimeout)
	defer cancel()
	if len(c.pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid VSP Ed25519 public key length")
	}
	var reqBody io.Reader
	var sig []byte
	if method == "POST" {
		body, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		sig, err = c.sign(ctx, string(body), addr)
		if err != nil {
			return fmt.Errorf("sign request: %w", err)
		}
		reqBody = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.url+path, reqBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if sig != nil {
		httpReq.Header.Set("VSP-Client-Signature", base64.StdEncoding.EncodeToString(sig))
	}
	reply, err := c.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, httpReq.URL.String(), err)
	}
	defer reply.Body.Close()

	status := reply.StatusCode
	is200 := status == 200
	is4xx := status >= 400 && status <= 499
	if !(is200 || is4xx) {
		return fmt.Errorf("%s %s: http %v %s", method, httpReq.URL.String(),
			status, http.StatusText(status))
	}
	sigBase64 := reply.Header.Get("VSP-Server-Signature")
	if sigBase64 == "" {
		return fmt.Errorf("cannot authenticate server: no signature")
	}
	sig, err = base64.StdEncoding.DecodeString(sigBase64)
	if err != nil {
		return fmt.Errorf("cannot authenticate server: %w", err)
	}
	respBody, err := io.ReadAll(io.LimitReader(reply.Body, maxVSPResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if len(respBody) > maxVSPResponseBytes {
		return fmt.Errorf("VSP response exceeds %d bytes", maxVSPResponseBytes)
	}
	if !ed25519.Verify(c.pub, respBody, sig) {
		return fmt.Errorf("cannot authenticate server: invalid signature")
	}
	var apiError *BadRequestError
	if is4xx {
		apiError = new(BadRequestError)
		resp = apiError
	}
	if resp != nil {
		err = json.Unmarshal(respBody, resp)
		if err != nil {
			return fmt.Errorf("unmarshal respose body: %w", err)
		}
	}
	if apiError != nil {
		apiError.HTTPStatus = status
		return apiError
	}
	return nil
}
