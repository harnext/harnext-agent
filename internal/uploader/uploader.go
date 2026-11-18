// Package uploader sends session aggregates to POST /api/v1/ingest. Only rows whose content
// changed since the last accepted upload are sent; the server upserts, so retries are safe.
// Rows carry counters and labels only; errors never include request or response content.
package uploader

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

const (
	ingestPath         = "/api/v1/ingest"
	DefaultPeriod      = 5 * time.Minute
	DefaultHTTPTimeout = 30 * time.Second
	// DefaultMaxBodyBytes stays below the server's 1 MB limit.
	DefaultMaxBodyBytes = 900 * 1024
	// DefaultBatchPause spaces full batches under the server's 2000 sessions per minute.
	DefaultBatchPause = 6 * time.Second
	maxResponseBytes  = 1 << 20
	maxRetryAfter     = time.Hour
	baseBackoff       = time.Minute
	jitterFraction    = 0.1
	codeFutureTime    = "future_timestamp"
)

type Code string

const (
	CodeNoToken      Code = "no_token"
	CodeBadURL       Code = "bad_url"
	CodeUnauthorized Code = "unauthorized"
	// CodeTransient covers network failures, 429 and 5xx; retry later.
	CodeTransient Code = "transient"
	// CodeRejected is a permanent refusal of the whole request (bad version, too large, 4xx).
	CodeRejected  Code = "rejected"
	CodeMalformed Code = "malformed_response"
	CodeLedger    Code = "ledger"
)

// Error is content-free.
type Error struct {
	Code              Code
	RetryAfterSeconds int
}

func (e *Error) Error() string { return "uploader: " + string(e.Code) }

// Ledger remembers which row content the server has accepted. *storage.Store satisfies it.
type Ledger interface {
	UploadedDigest(rowKey string) (string, bool, error)
	MarkUploaded(digests map[string]string, at time.Time) error
}

type Options struct {
	BaseURL      string        // https://host[/prefix]; no query, fragment or user info
	Token        func() string // called per request, never logged
	AgentVersion string        // 1..32 chars
	Ledger       Ledger        // required
	// AllowInsecureLocalhost permits http:// for localhost, 127.0.0.1 and ::1 (development only).
	AllowInsecureLocalhost bool
	HTTPClient             *http.Client
	MaxBodyBytes           int
	BatchPause             time.Duration
	Now                    func() time.Time
	Sleep                  func(ctx context.Context, d time.Duration) error
	Rand                   func() float64 // [0,1)
	// OnBatch, if set, is called after each accepted batch with the totals of the running Sync.
	OnBatch func(Result)
	// Catchup, if set, is asked before each Run iteration whether more rows are about to arrive
	// (history is still being read). While true, a successful iteration is followed by
	// BatchPause instead of the full period, so the backlog drains at the server's pace.
	Catchup func() bool
	// Backlog, if set, reports how many sessions are not on the server yet. It is sent with every ingest request as
	// X-Harnext-Backlog (a bare count, no content) so the panel can show that history is still syncing; 0 clears it.
	Backlog func() int
	// BacklogTotal, if set, reports how many sessions the first scan found (last 30 days). It is sent as
	// X-Harnext-Backlog-Total next to Backlog so the panel can show a percentage; a bare count, no content.
	BacklogTotal func() int
	// NotBeforeDay (YYYY-MM-DD, UTC; empty = none) drops usage rows of earlier days before anything is sent. It is set after the
	// token changed, so work done for the previous organization is not uploaded to the new one (day precision: the switch day
	// itself is included). A session left without usage rows is not sent.
	NotBeforeDay string
}

type Uploader struct {
	endpoint string
	opts     Options
	http     *http.Client
}

// Result counts what one Sync did.
type Result struct {
	Accepted  int // rows the server stored
	Rejected  int // rows the server refused permanently; not retried until their content changes
	Deferred  int // rows refused for a transient reason (future timestamp); retried next time
	Unchanged int // rows skipped because the server already has this content
	Invalid   int // rows outside the contract limits; never sent
	Batches   int
	Remaining int // rows still queued after the last batch of this Sync (0 when it finished)
}

func New(opts Options) (*Uploader, error) {
	if opts.Token == nil || opts.Ledger == nil {
		return nil, &Error{Code: CodeNoToken}
	}
	if n := len(opts.AgentVersion); n < 1 || n > 32 {
		return nil, &Error{Code: CodeBadURL}
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, &Error{Code: CodeBadURL}
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !opts.AllowInsecureLocalhost || !isLoopbackHost(u.Hostname()) {
			return nil, &Error{Code: CodeBadURL}
		}
	default:
		return nil, &Error{Code: CodeBadURL}
	}
	hc := &http.Client{}
	if opts.HTTPClient != nil {
		cp := *opts.HTTPClient
		hc = &cp
	}
	if hc.Timeout <= 0 {
		hc.Timeout = DefaultHTTPTimeout
	}
	// The Authorization header must never follow a redirect to another host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.BatchPause <= 0 {
		opts.BatchPause = DefaultBatchPause
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	if opts.Rand == nil {
		opts.Rand = rand.Float64
	}
	return &Uploader{endpoint: strings.TrimRight(u.String(), "/") + ingestPath, opts: opts, http: hc}, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Sync uploads the rows whose content the server does not have yet, newest session first, so
// the most recent history is on the server before the older one. configVersion is the
// version of the signed configuration that produced the rows' scores (0 = none; omitted).
// Batches already accepted stay recorded when a later batch fails, so a retry resumes where
// it stopped.
func (u *Uploader) Sync(ctx context.Context, rows []aggregate.Row, configVersion int64) (Result, error) {
	rows = newestSessionsFirst(dropBeforeDay(rows, u.opts.NotBeforeDay))
	valid, invalid := prepare(rows, configVersion)
	res := Result{Invalid: invalid}
	var pending []prepared
	for _, p := range valid {
		d, ok, err := u.opts.Ledger.UploadedDigest(p.key)
		if err != nil {
			return res, &Error{Code: CodeLedger}
		}
		if ok && d == p.digest {
			res.Unchanged++
			continue
		}
		pending = append(pending, p)
	}

	res.Remaining = len(pending)
	for len(pending) > 0 {
		if res.Batches > 0 {
			if err := u.opts.Sleep(ctx, u.opts.BatchPause); err != nil {
				return res, err
			}
		}
		n, size := 0, 0
		for n < len(pending) && n < MaxSessionsPerPost {
			size += len(pending[n].body) + 1
			if n > 0 && size > u.opts.MaxBodyBytes {
				break
			}
			n++
		}
		batch := pending[:n]
		pending = pending[n:]
		res.Remaining = len(pending) + n // the batch counts as queued until the server answers

		rowErrs, err := u.post(ctx, batch, configVersion)
		if err != nil {
			return res, err
		}
		res.Batches++
		res.Remaining = len(pending)
		mark := make(map[string]string, len(batch))
		for i, p := range batch {
			switch code, bad := rowErrs[i]; {
			case !bad:
				res.Accepted++
				mark[p.key] = p.digest
			case code == codeFutureTime:
				res.Deferred++
			default:
				res.Rejected++
				mark[p.key] = p.digest
			}
		}
		if len(mark) > 0 {
			if err := u.opts.Ledger.MarkUploaded(mark, u.opts.Now()); err != nil {
				return res, &Error{Code: CodeLedger}
			}
		}
		if u.opts.OnBatch != nil {
			u.opts.OnBatch(res)
		}
	}
	return res, nil
}

// newestSessionsFirst orders rows by the end of their session, newest first. The order inside
// a session is kept (the body and digest of a session depend on it).
func newestSessionsFirst(rows []aggregate.Row) []aggregate.Row {
	ended := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		k := rowKey(r)
		if r.EndedAt.After(ended[k]) {
			ended[k] = r.EndedAt
		}
	}
	out := slices.Clone(rows)
	slices.SortStableFunc(out, func(a, b aggregate.Row) int { return ended[rowKey(b)].Compare(ended[rowKey(a)]) })
	return out
}

type ingestResponse struct {
	Error  string `json:"error"`
	Errors []struct {
		Index int    `json:"index"`
		Code  string `json:"code"`
	} `json:"errors"`
}

// encodeBody is the request body of one ingest call; post and Preview share it so a preview is what would be sent.
func encodeBody(agentVersion string, configVersion int64, batch []prepared) []byte {
	var buf bytes.Buffer
	head, _ := json.Marshal(agentVersion)
	buf.WriteString(`{"payload_version":` + strconv.Itoa(PayloadVersion) + `,"agent_version":`)
	buf.Write(head)
	if configVersion >= 1 {
		buf.WriteString(`,"config_version":`)
		buf.WriteString(strconv.FormatInt(configVersion, 10))
	}
	buf.WriteString(`,"sessions":[`)
	for i, p := range batch {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(p.body)
	}
	buf.WriteString(`]}`)
	return buf.Bytes()
}

// post sends one batch and returns the per-row error codes by batch index.
func (u *Uploader) post(ctx context.Context, batch []prepared, configVersion int64) (map[int]string, error) {
	tok := u.opts.Token()
	if tok == "" {
		return nil, &Error{Code: CodeNoToken}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint, bytes.NewReader(encodeBody(u.opts.AgentVersion, configVersion, batch)))
	if err != nil {
		return nil, &Error{Code: CodeBadURL}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if u.opts.Backlog != nil {
		req.Header.Set("X-Harnext-Backlog", strconv.Itoa(max(u.opts.Backlog(), 0)))
	}
	if u.opts.BacklogTotal != nil {
		req.Header.Set("X-Harnext-Backlog-Total", strconv.Itoa(max(u.opts.BacklogTotal(), 0)))
	}

	resp, err := u.http.Do(req)
	if err != nil {
		// *url.Error embeds the request URL; drop it.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: CodeTransient}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusBadRequest:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &Error{Code: CodeTransient}
		}
		var r ingestResponse
		if json.Unmarshal(body, &r) != nil {
			return nil, &Error{Code: CodeMalformed}
		}
		// 400 with row errors and no error name: every row was refused individually.
		if resp.StatusCode == http.StatusBadRequest && (r.Error != "" || len(r.Errors) == 0) {
			return nil, &Error{Code: CodeRejected}
		}
		out := make(map[int]string, len(r.Errors))
		for _, e := range r.Errors {
			if e.Index >= 0 && e.Index < len(batch) {
				out[e.Index] = e.Code
			}
		}
		return out, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &Error{Code: CodeUnauthorized}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &Error{Code: CodeTransient, RetryAfterSeconds: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode >= 500:
		return nil, &Error{Code: CodeTransient}
	default:
		return nil, &Error{Code: CodeRejected}
	}
}

func parseRetryAfter(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	if max := int(maxRetryAfter / time.Second); n > max {
		return max
	}
	return n
}

// Run syncs every period (with jitter) until ctx ends. source returns the rows and the
// configuration version they were computed with, read together. After a transient failure
// it backs off from one minute, doubling up to period, and honors Retry-After. report may be nil.
func (u *Uploader) Run(ctx context.Context, source func() ([]aggregate.Row, int64), period time.Duration, report func(Result, error)) {
	if period <= 0 {
		period = DefaultPeriod
	}
	failures := 0
	for {
		catchup := u.opts.Catchup != nil && u.opts.Catchup() // before the rows are read
		rows, cv := source()
		res, err := u.Sync(ctx, rows, cv)
		if ctx.Err() != nil {
			return
		}
		if report != nil {
			report(res, err)
		}
		wait := u.nextWait(period, err, &failures)
		if catchup && err == nil {
			wait = u.opts.BatchPause
		}
		if u.opts.Sleep(ctx, wait) != nil {
			return
		}
	}
}

func (u *Uploader) nextWait(period time.Duration, err error, failures *int) time.Duration {
	if err == nil {
		*failures = 0
		return period + time.Duration(u.opts.Rand()*jitterFraction*float64(period))
	}
	wait := period
	if e, ok := err.(*Error); ok && e.Code == CodeTransient {
		wait = baseBackoff << min(*failures, 20)
		if wait > period {
			wait = period
		}
		if ra := time.Duration(e.RetryAfterSeconds) * time.Second; ra > wait {
			wait = ra
		}
	}
	*failures++
	return wait
}
