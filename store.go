package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"
)

type kv interface {
	Ping(ctx context.Context) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Del(ctx context.Context, key string) error
	CAS(ctx context.Context, key string, expected []byte, expectFound bool, value []byte) (bool, error)
	SetTTL(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Expire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, bool, error)
	Incr(ctx context.Context, key string, delta int64) (int64, error)
}

type link struct {
	URL     string `json:"url"`
	Created int64  `json:"created"`
	Secret  string `json:"secret"`
	Removed bool   `json:"removed,omitempty"`
}

const (
	alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	codeLen     = 7
	claimTries  = 5
	maxReported = 500
	reportedKey = "reported"
)

var errNoCode = errors.New("no free code after several tries")

func linkKey(code string) string    { return "link/" + code }
func clicksKey(code string) string  { return "clicks/" + code }
func reportsKey(code string) string { return "reports/" + code }

func validCode(s string) bool {
	if len(s) != codeLen {
		return false
	}
	for _, c := range s {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// randomCode draws from r with rejection sampling: 248 is the largest multiple of 62 that fits in a byte
func randomCode(r io.Reader) (string, error) {
	out := make([]byte, 0, codeLen)
	var b [1]byte
	for len(out) < codeLen {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", fmt.Errorf("read random: %w", err)
		}
		if b[0] < 248 {
			out = append(out, alphabet[int(b[0])%len(alphabet)])
		}
	}
	return string(out), nil
}

func newCode() (string, error) { return randomCode(rand.Reader) }

func newSecret() (secret, hash string, err error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("read random: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(b[:])
	return secret, hashSecret(secret), nil
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func secretMatches(stored, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(hashSecret(secret))) == 1
}

func (a *app) getLink(ctx context.Context, code string) (l link, found bool, err error) {
	raw, ok, err := a.kv.Get(ctx, linkKey(code))
	if err != nil || !ok {
		return link{}, false, err
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return link{}, false, fmt.Errorf("decode link %s: %w", code, err)
	}
	return l, true, nil
}

// claim stores l under a fresh code with CAS, so a code that is taken is never overwritten
func (a *app) claim(ctx context.Context, l link, ttl time.Duration) (string, error) {
	val, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("encode link: %w", err)
	}
	for range claimTries {
		code, err := a.codes()
		if err != nil {
			return "", err
		}
		swapped, err := a.kv.CAS(ctx, linkKey(code), nil, false, val)
		if err != nil {
			return "", fmt.Errorf("claim %s: %w", code, err)
		}
		if !swapped {
			continue
		}
		if ttl > 0 {
			if err := a.setExpiry(ctx, code, ttl); err != nil {
				if derr := a.deleteLink(context.WithoutCancel(ctx), code); derr != nil {
					err = errors.Join(err, derr)
				}
				return "", err
			}
		}
		return code, nil
	}
	return "", errNoCode
}

// setExpiry gives the counters the link's deadline up front, since Incr keeps a deadline
func (a *app) setExpiry(ctx context.Context, code string, ttl time.Duration) error {
	for _, k := range []string{clicksKey(code), reportsKey(code)} {
		if err := a.kv.SetTTL(ctx, k, []byte("0"), ttl); err != nil {
			return fmt.Errorf("expire %s: %w", k, err)
		}
	}
	live, err := a.kv.Expire(ctx, linkKey(code), ttl)
	if err != nil {
		return fmt.Errorf("expire link %s: %w", code, err)
	}
	if !live {
		return fmt.Errorf("expire link %s: key is gone", code)
	}
	return nil
}

// deleteLink removes the link first, so it stops serving even if a counter delete fails
func (a *app) deleteLink(ctx context.Context, code string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var errs []error
	for _, k := range []string{linkKey(code), clicksKey(code), reportsKey(code)} {
		if err := a.kv.Del(ctx, k); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}

func (a *app) counter(ctx context.Context, key string) (int64, error) {
	raw, ok, err := a.kv.Get(ctx, key)
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode counter %s: %w", key, err)
	}
	return n, nil
}

// update is a CAS loop on key: fn gets the current value and returns the new one, or nil to leave it
func (a *app) update(ctx context.Context, key string, fn func(old []byte, found bool) ([]byte, error)) error {
	for {
		old, found, err := a.kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("get %s: %w", key, err)
		}
		val, err := fn(old, found)
		if err != nil || val == nil {
			return err
		}
		swapped, err := a.kv.CAS(ctx, key, old, found, val)
		if err != nil {
			return fmt.Errorf("cas %s: %w", key, err)
		}
		if swapped {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("update %s: %w", key, err)
		}
	}
}

// takedown marks the link removed. The swap is a set, so it also clears the deadline: the code keeps answering 410
func (a *app) takedown(ctx context.Context, code string) (found bool, err error) {
	err = a.update(ctx, linkKey(code), func(old []byte, ok bool) ([]byte, error) {
		found = ok
		if !ok {
			return nil, nil
		}
		var l link
		if err := json.Unmarshal(old, &l); err != nil {
			return nil, fmt.Errorf("decode link %s: %w", code, err)
		}
		if l.Removed {
			return nil, nil
		}
		l.Removed = true
		return json.Marshal(l)
	})
	return found, err
}

func (a *app) addReported(ctx context.Context, code string) error {
	return a.update(ctx, reportedKey, func(old []byte, found bool) ([]byte, error) {
		var codes []string
		if found {
			if err := json.Unmarshal(old, &codes); err != nil {
				return nil, fmt.Errorf("decode %s: %w", reportedKey, err)
			}
		}
		if slices.Contains(codes, code) {
			return nil, nil
		}
		codes = append([]string{code}, codes...)
		if len(codes) > maxReported {
			codes = codes[:maxReported]
		}
		return json.Marshal(codes)
	})
}

func (a *app) reportedCodes(ctx context.Context) ([]string, error) {
	raw, ok, err := a.kv.Get(ctx, reportedKey)
	if err != nil || !ok {
		return nil, err
	}
	var codes []string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &codes); err != nil {
		return nil, fmt.Errorf("decode %s: %w", reportedKey, err)
	}
	return codes, nil
}
