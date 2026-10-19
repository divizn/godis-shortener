package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const maxURLLen = 2048

// validateURL returns the canonical form of raw, refusing anything that is not a plain absolute http(s) link away from self
func validateURL(raw string, self *url.URL) (string, error) {
	if len(raw) > maxURLLen {
		return "", fmt.Errorf("url is longer than %d bytes", maxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("url is not valid")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme != "http" && scheme != "https":
		return "", errors.New("url must start with http:// or https://")
	case u.Hostname() == "":
		return "", errors.New("url has no host")
	case u.User != nil:
		return "", errors.New("url must not contain a username or password")
	case hostKey(u) == hostKey(self):
		return "", errors.New("url points back to this service")
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// hostKey is the lowercase host without a trailing dot, plus the port, which defaults from the scheme
func hostKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") + ":" + port
}
