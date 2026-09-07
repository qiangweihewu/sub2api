package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Regression coverage for: a group with many Bedrock/Anthropic accounts (one
// per region) where one account is TCP-unreachable. Before this fix, a
// transport-level failure (DoWithTLS returning err, not an HTTP response) was
// written straight to the client as a plain 502 and returned as a plain error,
// which the handler's failover loop does not recognize — the dead account
// never got benched and stayed IsSchedulable()==true forever, so sticky
// sessions (and round-robin scheduling) kept sending traffic to it.

// transportErrHTTPUpstream always fails the round trip at the transport level
// (e.g. "dial tcp ...: connect: no route to host") — it never returns an
// *http.Response, mirroring what net/http does for a dead TCP endpoint.
type transportErrHTTPUpstream struct {
	err   error
	calls int
}

func (u *transportErrHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return nil, u.err
}

func (u *transportErrHTTPUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	return nil, u.err
}

func (u *transportErrHTTPUpstream) Prewarm(_ context.Context, _ string, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) error {
	return nil
}

// transportErrTempStepCall records a SetTempUnschedulableWithStep invocation.
type transportErrTempStepCall struct {
	accountID int64
	until     time.Time
	reason    string
	step      int
}

// transportErrAccountRepoStub is a minimal AccountRepository fake: GetByID
// serves a single in-memory account, SetTempUnschedulableWithStep records
// calls. Any other method panics (embedded nil interface) — the code under
// test must only touch these two.
type transportErrAccountRepoStub struct {
	AccountRepository
	account   *Account
	stepCalls []transportErrTempStepCall
}

func (r *transportErrAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account != nil && r.account.ID == id {
		return r.account, nil
	}
	return nil, errors.New("account not found")
}

func (r *transportErrAccountRepoStub) SetTempUnschedulableWithStep(_ context.Context, id int64, until time.Time, reason string, step int) error {
	r.stepCalls = append(r.stepCalls, transportErrTempStepCall{accountID: id, until: until, reason: reason, step: step})
	return nil
}

// SetTempUnschedulable records into the same slice as the WithStep variant.
// After the 0.2.1 sync the Anthropic/Bedrock transport-error path goes through
// the shared GatewayService.handleUpstreamTransportError, which benches with a
// fixed 10-minute window via SetTempUnschedulable instead of the fork's older
// step-backoff call. Recording both into stepCalls keeps every existing
// assertion (count / account id / until / reason) meaningful regardless of
// which variant the production path picks; step is 0 for the plain form.
func (r *transportErrAccountRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.stepCalls = append(r.stepCalls, transportErrTempStepCall{accountID: id, until: until, reason: reason})
	return nil
}

func newTransportErrTestAccount() *Account {
	return &Account{
		ID:          9001,
		Name:        "bedrock-me-south-1",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "oauth-token",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func newTransportErrTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func newTransportErrTestParsed(model string) *ParsedRequest {
	body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	return &ParsedRequest{Body: NewRequestBodyRef(body), Model: model}
}

// TestForward_TransportError_ReturnsFailoverErrorForAccountSwitch is the core
// regression test for the bug: a transport-level upstream failure must come
// back as *UpstreamFailoverError (not a plain error) so the handler's failover
// loop switches to the next account, and it must not be retryable on the same
// (permanently dead) account.
func TestForward_TransportError_ReturnsFailoverErrorForAccountSwitch(t *testing.T) {
	account := newTransportErrTestAccount()
	repo := &transportErrAccountRepoStub{account: account}
	upstream := &transportErrHTTPUpstream{
		err: errors.New(`Post "https://bedrock-runtime.me-south-1.amazonaws.com/model/x/invoke": dial tcp 10.20.30.40:443: connect: no route to host`),
	}

	svc := &GatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		accountRepo:      repo,
		rateLimitService: &RateLimitService{accountRepo: repo},
	}

	c, rec := newTransportErrTestContext()
	result, err := svc.Forward(context.Background(), c, account, newTransportErrTestParsed("claude-3-5-sonnet-latest"))
	require.Nil(t, result)

	var fo *UpstreamFailoverError
	require.True(t, errors.As(err, &fo), "transport-level error must return *UpstreamFailoverError so the handler can fail over")
	require.False(t, fo.RetryableOnSameAccount, "must not be retried in place — retrying a permanently dead account wastes a round trip instead of switching")
	require.Equal(t, http.StatusBadGateway, fo.StatusCode)

	// The service must not write a response itself: the handler owns the final
	// response, whether that's a switch to the next account or (once failover
	// is exhausted) a single terminal error.
	require.Equal(t, 0, rec.Body.Len(), "Forward must not write a hard response on a failover-triggering transport error")
	require.Equal(t, 1, upstream.calls)
}

// TestForward_TransportError_TempUnschedulesAccount verifies the dead account
// is benched (temp-unscheduled) so it leaves the schedulable pool instead of
// keeping IsSchedulable()==true forever.
func TestForward_TransportError_TempUnschedulesAccount(t *testing.T) {
	account := newTransportErrTestAccount()
	repo := &transportErrAccountRepoStub{account: account}
	upstream := &transportErrHTTPUpstream{err: errors.New(`dial tcp: connect: connection refused`)}

	svc := &GatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		accountRepo:      repo,
		rateLimitService: &RateLimitService{accountRepo: repo},
	}

	c, _ := newTransportErrTestContext()
	before := time.Now()
	_, err := svc.Forward(context.Background(), c, account, newTransportErrTestParsed("claude-3-5-sonnet-latest"))
	require.Error(t, err)

	require.Len(t, repo.stepCalls, 1, "transport failure must temp-unschedule the account exactly once")
	call := repo.stepCalls[0]
	require.Equal(t, account.ID, call.accountID)
	require.True(t, call.until.After(before), "temp-unschedule window must be in the future")
	require.Contains(t, call.reason, "connection refused")

	// Simulate the persisted state round-tripping back onto the account entity
	// (what the scheduler snapshot/sticky-session lookup would see on the next
	// request) and verify it now reports as unschedulable — this is exactly
	// the condition shouldClearStickySession checks to evict a sticky binding.
	account.TempUnschedulableUntil = &call.until
	require.False(t, account.IsSchedulable(), "account must leave the schedulable pool after a transport failure")
}

// TestForward_TransportError_ContextCanceled_NoFailoverNoEviction verifies a
// client disconnect (context.Canceled) is not misclassified as an upstream
// fault: no failover attempt on another account, and no eviction of this one.
func TestForward_TransportError_ContextCanceled_NoFailoverNoEviction(t *testing.T) {
	account := newTransportErrTestAccount()
	repo := &transportErrAccountRepoStub{account: account}
	upstream := &transportErrHTTPUpstream{err: context.Canceled}

	svc := &GatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		accountRepo:      repo,
		rateLimitService: &RateLimitService{accountRepo: repo},
	}

	c, rec := newTransportErrTestContext()
	_, err := svc.Forward(context.Background(), c, account, newTransportErrTestParsed("claude-3-5-sonnet-latest"))

	var fo *UpstreamFailoverError
	require.False(t, errors.As(err, &fo), "client disconnect must not trigger account failover")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, repo.stepCalls, "client disconnect must not temp-unschedule the account")
	require.Equal(t, 0, rec.Body.Len())
}

// ---------------------------------------------------------------------------
// upstream 44003d7f6: direct unit coverage for the shared
// GatewayService.handleUpstreamTransportError helper (transient vs persistent
// classification, Ops proxy attribution, client-cancel and upstream-deadline).
// Merged into this file because both sides created it independently.
// ---------------------------------------------------------------------------

type transportTempUnschedRepoStub struct {
	AccountRepository
	calls      int
	lastID     int64
	lastUntil  time.Time
	lastReason string
}

func (r *transportTempUnschedRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.calls++
	r.lastID = id
	r.lastUntil = until
	r.lastReason = reason
	return nil
}

func newTransportErrorTestGin(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

// TestHandleUpstreamTransportError_TransientFailsOverWithoutEviction pins the
// contract for transient transport blips (EOF / connection reset): the request
// fails over to another account, the current account stays schedulable, and
// nothing is written to the response (the handler owns it).
func TestHandleUpstreamTransportError_TransientFailsOverWithoutEviction(t *testing.T) {
	repo := &transportTempUnschedRepoStub{}
	s := &GatewayService{accountRepo: repo}
	c := newTransportErrorTestGin(t)
	account := &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic}

	err := s.handleUpstreamTransportError(context.Background(), c, account,
		errors.New(`Post "http://upstream/v1/messages?beta=true": EOF`), OpsUpstreamErrorEvent{})

	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("expected *UpstreamFailoverError, got %T: %v", err, err)
	}
	if failoverErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d, want 502", failoverErr.StatusCode)
	}
	if string(failoverErr.ResponseBody) != string(gatewayTransportFailoverBody) {
		t.Fatalf("ResponseBody = %s, want legacy 502 body", failoverErr.ResponseBody)
	}
	if !failoverErr.ShouldRetryNextAccount() {
		t.Fatal("transient transport error must allow retrying the next account")
	}
	if repo.calls != 0 {
		t.Fatalf("SetTempUnschedulable called %d times for a transient error, want 0", repo.calls)
	}
	if c.Writer.Written() {
		t.Fatal("handler owns the response; service must not write on transport failover")
	}
}

// TestHandleUpstreamTransportError_AttributesEventTimeProxy pins that the
// shared helper stamps proxy attribution from the same account snapshot the
// transport used, so every Anthropic/Bedrock caller inherits it.
func TestHandleUpstreamTransportError_AttributesEventTimeProxy(t *testing.T) {
	proxyID := int64(10060)
	tests := []struct {
		name     string
		account  *Account
		wantID   *int64
		wantName string
	}{
		{
			name:     "managed proxy",
			account:  &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Name: "wldsg82-ipv6-10060"}},
			wantID:   &proxyID,
			wantName: "wldsg82-ipv6-10060",
		},
		{
			name:     "direct",
			account:  &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic},
			wantName: opsProxyNameDirect,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &GatewayService{accountRepo: &transportTempUnschedRepoStub{}}
			c := newTransportErrorTestGin(t)
			_ = s.handleUpstreamTransportError(context.Background(), c, tt.account,
				errors.New("EOF"), OpsUpstreamErrorEvent{UpstreamURL: "https://api.anthropic.com/v1/messages", Passthrough: true})
			raw, ok := c.Get(OpsUpstreamErrorsKey)
			if !ok {
				t.Fatal("no upstream events recorded")
			}
			events := raw.([]*OpsUpstreamErrorEvent)
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			ev := events[0]
			if tt.wantID == nil {
				if ev.ProxyID != nil {
					t.Fatalf("proxy_id = %d, want null", *ev.ProxyID)
				}
			} else if ev.ProxyID == nil || *ev.ProxyID != *tt.wantID {
				t.Fatalf("proxy_id = %v, want %d", ev.ProxyID, *tt.wantID)
			}
			if ev.ProxyName != tt.wantName {
				t.Fatalf("proxy_name = %q, want %q", ev.ProxyName, tt.wantName)
			}
			if !ev.Passthrough || ev.UpstreamURL == "" || ev.Kind != "request_error" {
				t.Fatalf("caller-supplied fields lost: %+v", ev)
			}
		})
	}
}

// TestHandleUpstreamTransportError_PersistentEvictsAccount pins the contract
// for durable faults (dead endpoint / DNS / proxy credentials): fail over AND
// temporarily unschedule the account for the transport cooldown.
func TestHandleUpstreamTransportError_PersistentEvictsAccount(t *testing.T) {
	repo := &transportTempUnschedRepoStub{}
	s := &GatewayService{accountRepo: repo}
	c := newTransportErrorTestGin(t)
	account := &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic}

	before := time.Now()
	err := s.handleUpstreamTransportError(context.Background(), c, account,
		errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`), OpsUpstreamErrorEvent{})

	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("expected *UpstreamFailoverError, got %T: %v", err, err)
	}
	if repo.calls != 1 {
		t.Fatalf("SetTempUnschedulable called %d times for a persistent error, want 1", repo.calls)
	}
	if repo.lastID != account.ID {
		t.Fatalf("unscheduled account = %d, want %d", repo.lastID, account.ID)
	}
	wantUntil := before.Add(gatewayTransportErrorTempUnschedDuration)
	if repo.lastUntil.Before(wantUntil.Add(-time.Minute)) || repo.lastUntil.After(wantUntil.Add(time.Minute)) {
		t.Fatalf("until = %v, want ~%v", repo.lastUntil, wantUntil)
	}
	if !strings.HasPrefix(repo.lastReason, "upstream transport error (proxy/network): ") {
		t.Fatalf("reason = %q, want transport-error prefix", repo.lastReason)
	}
}

// TestHandleUpstreamTransportError_ClientCanceledNoFailover pins that a
// canceled client neither fails over nor evicts: the upstream never had a
// chance to exhibit a fault.
func TestHandleUpstreamTransportError_ClientCanceledNoFailover(t *testing.T) {
	repo := &transportTempUnschedRepoStub{}
	s := &GatewayService{accountRepo: repo}
	c := newTransportErrorTestGin(t)
	account := &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic}

	inErr := context.Canceled
	err := s.handleUpstreamTransportError(context.Background(), c, account, inErr, OpsUpstreamErrorEvent{})

	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		t.Fatal("canceled client must not fail over to another account")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled passthrough", err)
	}
	if repo.calls != 0 {
		t.Fatalf("SetTempUnschedulable called %d times on client cancel, want 0", repo.calls)
	}
}

// TestHandleUpstreamTransportError_UpstreamDeadlineStillFailsOver pins that an
// upstream-side timeout (request context still alive) is treated as a
// transient fault: fail over, no eviction.
func TestHandleUpstreamTransportError_UpstreamDeadlineStillFailsOver(t *testing.T) {
	repo := &transportTempUnschedRepoStub{}
	s := &GatewayService{accountRepo: repo}
	c := newTransportErrorTestGin(t)
	account := &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic}

	err := s.handleUpstreamTransportError(context.Background(), c, account,
		context.DeadlineExceeded, OpsUpstreamErrorEvent{})

	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("upstream deadline with live request context must fail over, got %T: %v", err, err)
	}
	if repo.calls != 0 {
		t.Fatalf("SetTempUnschedulable called %d times for upstream deadline, want 0", repo.calls)
	}
}
