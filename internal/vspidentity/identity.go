package vspidentity

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalHost validates and normalizes a provider's HTTPS base URL.
func CanonicalHost(host string) (string, error) {
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("invalid VSP URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || u.Opaque != "" || strings.Contains(host, "#") ||
		strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("VSP URL must use HTTPS without credentials, query, or fragment")
	}
	hostname := strings.ToLower(u.Hostname())
	u.Host = strings.ToLower(u.Host)
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid VSP port")
		}
		u.Host = net.JoinHostPort(hostname, strconv.Itoa(n))
		if n == 443 {
			u.Host = hostname
			if strings.Contains(hostname, ":") {
				u.Host = "[" + hostname + "]"
			}
		}
	}
	u.Scheme = "https"
	if u.RawPath == "" {
		u.Path = strings.TrimRight(u.Path, "/")
	} else {
		u.RawPath = strings.TrimRight(u.RawPath, "/")
		u.Path, _ = url.PathUnescape(u.RawPath)
	}
	return u.String(), nil
}
