package core

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HTTP tuning. The limits are deliberately generous for large artifacts but
// finite, so a stalled CDN fails instead of hanging a terminal forever.
// totalTimeout has to cover a genuinely huge OS image (a full macOS restore
// image runs well past 10GB) on an ordinary connection, not just a CLI
// tool's tarball.
const (
	dialTimeout     = 15 * time.Second
	headerTimeout   = 30 * time.Second
	idleConnTimeout = 90 * time.Second
	totalTimeout    = 3 * time.Hour
	maxRetries      = 3
)

// maxDownloadBytes bounds the raw network transfer for a single artifact —
// deliberately much larger than maxArchiveBytes in store.go, which bounds
// what an archive is allowed to *extract to*. An OS image artifact is never
// extracted at all, so it only needs to fit under this cap, not that one.
const maxDownloadBytes = 64 << 30 // 64 GiB

// NewHTTPClient builds the client hop uses for every network request.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: totalTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: headerTimeout,
			IdleConnTimeout:       idleConnTimeout,
			MaxIdleConnsPerHost:   8,
			ForceAttemptHTTP2:     true,
		},
	}
}

// BarHandle is the subset of a progress bar the fetcher drives. Keeping it an
// interface lets core stay free of any terminal dependency.
type BarHandle interface {
	io.Writer
	SetTotal(int64)
	Done(note string)
	Fail(reason string)
	Skip(note string)
}

// Sink receives progress from a batch of concurrent fetches.
type Sink interface {
	Bar(label string, total int64) BarHandle
	Log(format string, a ...any)
}

// discardSink satisfies Sink when no progress should be shown.
type discardSink struct{}

type discardBar struct{}

func (discardBar) Write(p []byte) (int, error) { return len(p), nil }
func (discardBar) SetTotal(int64)              {}
func (discardBar) Done(string)                 {}
func (discardBar) Fail(string)                 {}
func (discardBar) Skip(string)                 {}

func (discardSink) Bar(string, int64) BarHandle { return discardBar{} }
func (discardSink) Log(string, ...any)          {}

// DiscardSink is a Sink that reports nothing.
var DiscardSink Sink = discardSink{}

// FetchRequest is one artifact to download.
type FetchRequest struct {
	Name string // package name, used as the progress label
	URL  string
	// SHA256 and SHA512 are the expected digest; at most one is normally set
	// (SHA256 wins if both are, e.g. from a badly hand-edited recipe). Both
	// empty means trust-on-first-use.
	SHA256 string
	SHA512 string
	Size   int64 // expected size, for the progress bar before headers arrive
}

// pinnedDigest returns the digest this request pins and which algorithm it
// is, or ("", "") for trust-on-first-use.
func (r *FetchRequest) pinnedDigest() (algo, want string) {
	switch {
	case r.SHA256 != "":
		return "sha256", r.SHA256
	case r.SHA512 != "":
		return "sha512", r.SHA512
	default:
		return "", ""
	}
}

// FetchResult is the outcome of one download.
type FetchResult struct {
	Req    *FetchRequest
	Path   string // local file in the download cache
	SHA256 string // digests actually observed, both always computed
	SHA512 string
	Size   int64
	Cached bool
	Err    error
}

// downloadPath is the cache location for a request. Artifacts with a declared
// digest are keyed by content, so the same file shared by several recipes is
// downloaded once; undeclared ones are keyed by URL.
func downloadPath(l *Layout, r *FetchRequest) string {
	if _, want := r.pinnedDigest(); want != "" {
		return filepath.Join(l.Downloads(), want[:min(len(want), 64)])
	}
	h := sha256.Sum256([]byte(r.URL))
	return filepath.Join(l.Downloads(), "url-"+hex.EncodeToString(h[:8])+"-"+sanitiseName(baseName(r.URL)))
}

func sanitiseName(s string) string {
	if len(s) > 80 {
		s = s[:80]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// FetchAll downloads every request with at most jobs in flight, verifying
// digests as it goes. Results are returned in request order regardless of
// completion order, so callers can rely on the pairing.
func FetchAll(ctx context.Context, client *http.Client, l *Layout, reqs []*FetchRequest, jobs int, sink Sink) []*FetchResult {
	if sink == nil {
		sink = DiscardSink
	}
	if jobs < 1 {
		jobs = 1
	}
	results := make([]*FetchResult, len(reqs))

	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup

	for i, r := range reqs {
		wg.Add(1)
		go func(i int, r *FetchRequest) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = &FetchResult{Req: r, Err: ctx.Err()}
				return
			}

			bar := sink.Bar(r.Name, r.Size)
			res := fetchOne(ctx, client, l, r, bar)
			results[i] = res

			switch {
			case res.Err != nil:
				bar.Fail(shortError(res.Err))
			case res.Cached:
				bar.Skip("cached")
			default:
				bar.Done("")
			}
		}(i, r)
	}
	wg.Wait()
	return results
}

// fetchOne downloads and verifies a single artifact, retrying transient
// failures with backoff.
func fetchOne(ctx context.Context, client *http.Client, l *Layout, r *FetchRequest, bar BarHandle) *FetchResult {
	dst := downloadPath(l, r)
	algo, want := r.pinnedDigest()

	// Cache hit: only trust it if we can prove the contents.
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		if sha256hex, sha512hex, err := hashFileBoth(dst); err == nil {
			got := sha256hex
			if algo == "sha512" {
				got = sha512hex
			}
			if want == "" || strings.EqualFold(got, want) {
				return &FetchResult{Req: r, Path: dst, SHA256: sha256hex, SHA512: sha512hex, Size: fi.Size(), Cached: true}
			}
		}
		_ = os.Remove(dst) // corrupt or superseded
	}

	if err := os.MkdirAll(l.Downloads(), 0o755); err != nil {
		return &FetchResult{Req: r, Err: err}
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(attempt-1) * 800 * time.Millisecond
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return &FetchResult{Req: r, Err: ctx.Err()}
			}
		}

		sha256hex, sha512hex, size, err := download(ctx, client, r, dst, bar)
		if err == nil {
			got := sha256hex
			if algo == "sha512" {
				got = sha512hex
			}
			if want != "" && !strings.EqualFold(got, want) {
				// A digest mismatch is never retried: the bytes on the server
				// are not the bytes the recipe was written against.
				_ = os.Remove(dst)
				return &FetchResult{Req: r, Err: &DigestMismatch{
					Name: r.Name, URL: r.URL, Algo: algo, Want: want, Got: got,
				}}
			}
			return &FetchResult{Req: r, Path: dst, SHA256: sha256hex, SHA512: sha512hex, Size: size}
		}
		lastErr = err
		if !retryable(err) || ctx.Err() != nil {
			break
		}
	}
	_ = os.Remove(dst)
	return &FetchResult{Req: r, Err: lastErr}
}

// download streams one attempt to a temp file, hashing as it writes — both
// SHA-256 and SHA-512 in the same pass, since upstreams disagree on which
// one they publish — so the artifact is never read twice.
func download(ctx context.Context, client *http.Client, r *FetchRequest, dst string, bar BarHandle) (sha256hex, sha512hex string, size int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid URL %q: %w", r.URL, err)
	}
	req.Header.Set("User-Agent", userAgent())
	// Artifacts are already compressed; asking for gzip only wastes CPU and
	// would break Content-Length accounting for the progress bar.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", 0, &HTTPError{URL: r.URL, Status: resp.StatusCode, StatusText: resp.Status}
	}
	if resp.ContentLength > 0 {
		bar.SetTotal(resp.ContentLength)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".dl-*")
	if err != nil {
		return "", "", 0, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed
	}()

	// Preflight: refuse to start a download that cannot possibly fit.
	if resp.ContentLength > 0 {
		if free, err := freeSpace(filepath.Dir(dst)); err == nil && free > 0 && resp.ContentLength+(64<<20) > free {
			return "", "", 0, fmt.Errorf("need %s but only %s is free on %s",
				humanBytes(resp.ContentLength), humanBytes(free), filepath.Dir(dst))
		}
	}

	h256, h512 := sha256.New(), sha512.New()
	w := io.MultiWriter(tmp, h256, h512, bar)
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxDownloadBytes))
	if err != nil {
		return "", "", 0, err
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return "", "", 0, fmt.Errorf("truncated download: got %s of %s",
			humanBytes(n), humanBytes(resp.ContentLength))
	}
	if err := tmp.Sync(); err != nil {
		return "", "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", "", 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", "", 0, err
	}
	return hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h512.Sum(nil)), n, nil
}

// retryable distinguishes a flaky network from a wrong URL. Retrying a 404
// only wastes the user's time.
func retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= 500
	}
	var dm *DigestMismatch
	if errors.As(err, &dm) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true // timeouts, resets, DNS blips
}

// ------------------------------------------------------------------ errors ----

// HTTPError is a non-200 response.
type HTTPError struct {
	URL        string
	Status     int
	StatusText string
}

func (e *HTTPError) Error() string {
	switch e.Status {
	case http.StatusNotFound:
		return fmt.Sprintf("artifact not found (404): %s", e.URL)
	case http.StatusForbidden:
		return fmt.Sprintf("access denied (403): %s", e.URL)
	default:
		return fmt.Sprintf("server returned %s for %s", e.StatusText, e.URL)
	}
}

// DigestMismatch means the downloaded bytes are not what the recipe pinned.
// hop treats this as fatal and unrecoverable: it is either upstream tampering
// or a release that was replaced in place, and both deserve a human.
type DigestMismatch struct {
	Name string
	URL  string
	Algo string // "sha256" or "sha512"
	Want string
	Got  string
}

func (e *DigestMismatch) Error() string {
	algo := e.Algo
	if algo == "" {
		algo = "sha256"
	}
	return fmt.Sprintf("%s checksum mismatch for %s\n      expected %s\n      actual   %s\n      from     %s",
		algo, e.Name, e.Want, e.Got, e.URL)
}

// shortError trims an error to something that fits on a progress line.
func shortError(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusNotFound:
			return "not found (404)"
		case http.StatusForbidden:
			return "forbidden (403)"
		default:
			return fmt.Sprintf("HTTP %d", he.Status)
		}
	}
	var dm *DigestMismatch
	if errors.As(err, &dm) {
		return "checksum mismatch"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if len(s) > 48 {
		s = s[:47] + "…"
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
