package api

// Coverage for the usage accounting helpers in usage.go.
//
// These numbers are what per-tenant reporting serves, and per-tenant
// reporting is what operators bill and rate-limit against — so the tests
// below pin each behavior the helpers promise: attribution to the right
// tenant, folding repeated deltas into one pending batch, flushing once per
// documented period, draining on Stop, dropping (not re-queuing) an
// interval after a failed flush, the [1, maxUsageDays] bound on the
// reporting endpoint's day range, and race-free recording when request
// handlers and the flush loop run together.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/rpc"
	"github.com/sorotrail/sorotrail/internal/store"
)

// usageSpy wraps the package's fakeTenants and records how the server used
// it: how many batches were flushed, which day bucket was named, what
// day-range the reporting endpoint asked for, and (on demand) a failure to
// inject into the flush path. It mirrors the real deployment closely enough
// to matter — AddUsage is an UPSERT-shaped batch append, not an overwrite —
// and everything it touches is mutex-guarded because the concurrency test
// drives it from many goroutines at once, which the plain fake is not
// built for.
type usageSpy struct {
	*fakeTenants

	mu               sync.Mutex
	addUsageCalls    int
	lastDay          time.Time
	daysRequested    []int
	lastListedTenant int64
	failNext         bool
}

func newUsageSpy() *usageSpy {
	return &usageSpy{fakeTenants: newFakeTenants()}
}

func (s *usageSpy) AddUsage(_ context.Context, day time.Time, deltas map[int64]store.UsageDelta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addUsageCalls++
	s.lastDay = day
	if s.failNext {
		s.failNext = false
		return assert.AnError
	}
	for id, d := range deltas {
		s.usage[id] = append(s.usage[id], store.TenantUsage{
			TenantID: id, Requests: d.Requests,
			EventsServed: d.EventsServed, StreamSeconds: d.StreamSeconds,
		})
	}
	return nil
}

func (s *usageSpy) ListUsage(_ context.Context, id int64, days int) ([]store.TenantUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.daysRequested = append(s.daysRequested, days)
	s.lastListedTenant = id
	return append([]store.TenantUsage(nil), s.usage[id]...), nil
}

// rows returns the flushed usage rows for one tenant, copied so callers can
// assert without racing the flush loop.
func (s *usageSpy) rows(tenantID int64) []store.TenantUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.TenantUsage(nil), s.usage[tenantID]...)
}

func (s *usageSpy) flushCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addUsageCalls
}

func (s *usageSpy) daysSeen() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.daysRequested...)
}

// sumUsage totals a tenant's flushed rows, which is what a reporting
// endpoint ends up serving when several flush intervals have landed.
func sumUsage(rows []store.TenantUsage) (requests, events, seconds int64) {
	for _, r := range rows {
		requests += r.Requests
		events += r.EventsServed
		seconds += r.StreamSeconds
	}
	return requests, events, seconds
}

func usageTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newUsageTestServer builds a multi-tenant server whose usage store is the
// spy, so tests can call the recording helpers directly and observe what
// reaches the persistence boundary.
func newUsageTestServer(ts store.TenantStore) *Server {
	return New(&scopedStore{}, &stubRPC{health: rpc.Health{Status: "healthy"}},
		usageTestLogger(), "test-key").
		WithMultiTenancy(ts, MultiTenantOptions{})
}

// newUsageRouter is newUsageTestServer with its router built and three API
// keys minted: a plain tenant, a second tenant with no grants ("quiet"), and
// an admin. The quiet tenant's key is returned too, so a test can verify the
// zero-activity reporting path without minting its own credentials.
func newUsageRouter(t *testing.T) (http.Handler, *usageSpy, *Server, string, string, string) {
	t.Helper()
	// WithMultiTenancy flips process-wide caching flags; reset them so this
	// test does not leak state into the rest of the package's suite.
	SetTenantScopedCaching(false)
	t.Cleanup(func() { SetTenantScopedCaching(false) })

	spy := newUsageSpy()
	srv := newUsageTestServer(spy)
	keyA := spy.addTenant(t, store.Tenant{ID: 1, Name: "a", Enabled: true})
	keyQuiet := spy.addTenant(t, store.Tenant{ID: 2, Name: "quiet", Enabled: true})
	keyAdmin := spy.addTenant(t, store.Tenant{ID: 4, Name: "admin", Enabled: true, Admin: true})
	return srv.Router(), spy, srv, keyA, keyQuiet, keyAdmin
}

// usageGet issues an authenticated GET against the built router.
func usageGet(t *testing.T, router http.Handler, key, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestUsageRecorderRecordsAndFlushes pins the core aggregation: a recorded
// delta lands on exactly the tenant it names, repeated records for the same
// tenant fold into one batch, and nothing is written until Flush runs.
func TestUsageRecorderRecordsAndFlushes(t *testing.T) {
	t.Run("a recorded request increments the right tenant's counter", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Record(1, store.UsageDelta{Requests: 2, EventsServed: 4})
		rec.Record(2, store.UsageDelta{Requests: 1})
		rec.Flush(context.Background())

		require.Len(t, spy.rows(1), 1, "each flush must write one batched row per tenant")
		requests, events, seconds := sumUsage(spy.rows(1))
		assert.Equal(t, int64(2), requests)
		assert.Equal(t, int64(4), events)
		assert.Equal(t, int64(0), seconds, "only the fields the delta names move")

		requests, _, _ = sumUsage(spy.rows(2))
		assert.Equal(t, int64(1), requests, "tenant 2's counter must move independently")
		assert.Empty(t, spy.rows(3), "a tenant that recorded nothing must have no rows")
	})

	t.Run("repeated records for one tenant fold into a single delta", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Record(1, store.UsageDelta{EventsServed: 4})
		rec.Record(1, store.UsageDelta{EventsServed: 6})
		rec.Flush(context.Background())

		_, events, _ := sumUsage(spy.rows(1))
		assert.Equal(t, int64(10), events, "folding, not overwriting, is the whole point of the pending map")
	})

	t.Run("a zero tenant id and an empty delta are ignored", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Record(0, store.UsageDelta{Requests: 5})
		rec.Record(1, store.UsageDelta{})
		rec.Flush(context.Background())

		assert.Empty(t, spy.rows(0))
		assert.Empty(t, spy.rows(1), "an empty delta must not open a row for the tenant")
	})

	t.Run("flushing an empty recorder does not touch the store", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Flush(context.Background())

		assert.Zero(t, spy.flushCalls(), "an idle server must not issue no-op writes")
	})

	t.Run("flushes name a UTC day bucket", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Record(1, store.UsageDelta{Requests: 1})
		rec.Flush(context.Background())

		require.NotZero(t, spy.lastDay)
		assert.Equal(t, time.UTC, spy.lastDay.Location(),
			"the day bucket handed to the store must be UTC, matching the per-day reporting rows")
	})
}

// The nil-friendly construction is part of the contract: a recorder over a
// nil store (and a nil recorder itself) must be safe to hold and call, so
// single-tenant deployments need no nil checks at call sites.
func TestUsageRecorderDisabledIsANoop(t *testing.T) {
	t.Run("a recorder over a nil store never writes", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(nil, usageTestLogger(), time.Hour)

		rec.Record(1, store.UsageDelta{Requests: 1})
		rec.Flush(context.Background())
		rec.Start(context.Background())
		rec.Stop()

		assert.Zero(t, spy.flushCalls())
	})

	t.Run("a nil recorder is safe to call", func(t *testing.T) {
		var rec *UsageRecorder
		rec.Record(1, store.UsageDelta{Requests: 1})
		rec.Flush(context.Background())
		rec.Start(context.Background())
		rec.Stop()
	})
}

// TestUsageRecorderFlushPeriod covers the documented flushing period: a
// non-positive interval falls back to DefaultUsageFlushInterval, and the
// started loop drains whatever is pending roughly once per period until it
// is cancelled.
func TestUsageRecorderFlushPeriod(t *testing.T) {
	t.Run("a non-positive interval falls back to the documented default", func(t *testing.T) {
		for _, every := range []time.Duration{0, -time.Second} {
			rec := NewUsageRecorder(newUsageSpy(), usageTestLogger(), every)
			assert.Equal(t, DefaultUsageFlushInterval, rec.every,
				"interval %v must normalize to the default period", every)
		}
		rec := NewUsageRecorder(newUsageSpy(), usageTestLogger(), 40*time.Second)
		assert.Equal(t, 40*time.Second, rec.every, "a positive interval must be kept as given")
	})

	t.Run("the flush loop drains counters once per period", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), 25*time.Millisecond)

		rec.Record(1, store.UsageDelta{Requests: 3})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec.Start(ctx)

		require.Eventually(t, func() bool { return spy.flushCalls() >= 1 },
			2*time.Second, 10*time.Millisecond,
			"the first period must flush the pending counters")
		requests, _, _ := sumUsage(spy.rows(1))
		require.Equal(t, int64(3), requests, "draining must move the recorded counters, not lose them")

		// A second period with nothing recorded is a deliberate no-op, so
		// keep the loop fed and require the second tick to flush too.
		rec.Record(1, store.UsageDelta{Requests: 1})
		require.Eventually(t, func() bool {
			rec.Record(1, store.UsageDelta{Requests: 1})
			return spy.flushCalls() >= 2
		}, 2*time.Second, 10*time.Millisecond,
			"the second period must flush whatever arrived since the first")
		cancel()
		rec.wg.Wait()
	})

	t.Run("cancelling the context stops the loop", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), 25*time.Millisecond)

		rec.Record(1, store.UsageDelta{Requests: 1})
		ctx, cancel := context.WithCancel(context.Background())
		rec.Start(ctx)
		require.Eventually(t, func() bool { return spy.flushCalls() >= 1 },
			2*time.Second, 10*time.Millisecond, "the loop must flush before it can be observed stopping")

		before := spy.flushCalls()
		cancel()
		rec.wg.Wait()

		rec.Record(1, store.UsageDelta{Requests: 1})
		time.Sleep(100 * time.Millisecond) // four periods' worth
		assert.Equal(t, before, spy.flushCalls(), "a cancelled loop must not flush again")
	})
}

// Stop is the orderly-shutdown path: it drains the final interval so a
// graceful restart loses nothing, and it must be safe to call twice because
// shutdown handlers are not always the only caller.
func TestUsageRecorderStopDrainsPending(t *testing.T) {
	t.Run("stop flushes what is pending", func(t *testing.T) {
		spy := newUsageSpy()
		// An interval longer than the test: nothing may be flushed except
		// by Stop itself.
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Record(1, store.UsageDelta{Requests: 5})
		rec.Stop()

		requests, _, _ := sumUsage(spy.rows(1))
		assert.Equal(t, int64(5), requests, "an orderly shutdown must not discard the final interval")
	})

	t.Run("stop is safe to call twice", func(t *testing.T) {
		spy := newUsageSpy()
		rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

		rec.Record(1, store.UsageDelta{Requests: 5})
		rec.Stop()
		rec.Stop() // must not panic on a second close, nor double-write

		require.Len(t, spy.rows(1), 1, "the drained interval must be written exactly once")
	})
}

// A failed flush deliberately drops the interval rather than re-merging it,
// because re-merging would let a persistently failing database grow the
// pending map without bound. The test pins that tradeoff so it cannot be
// quietly reversed into a memory leak.
func TestUsageRecorderFlushFailureDropsTheInterval(t *testing.T) {
	spy := newUsageSpy()
	rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

	spy.failNext = true
	rec.Record(1, store.UsageDelta{Requests: 2})
	rec.Flush(context.Background()) // fails; counters must not be restored

	rec.Record(1, store.UsageDelta{Requests: 1})
	rec.Flush(context.Background())

	rows := spy.rows(1)
	require.Len(t, rows, 1, "only the successful flush may reach the store")
	requests, _, _ := sumUsage(rows)
	assert.Equal(t, int64(1), requests,
		"the failed interval must be dropped, not re-delivered on the next flush")
}

// Recording runs on every request path while the flush loop drains the same
// pending map, so the pair must be race-free. Run this test with -race; the
// totals pin that concurrent flushing never loses or duplicates a delta.
func TestUsageRecorderConcurrentRecording(t *testing.T) {
	spy := newUsageSpy()
	rec := NewUsageRecorder(spy, usageTestLogger(), time.Hour)

	const goroutines, perGoroutine = 8, 200
	var recorders sync.WaitGroup

	// A concurrent flusher mirrors the real deployment: the ticker drains
	// pending counters while request handlers keep folding deltas in.
	stop := make(chan struct{})
	var flusher sync.WaitGroup
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		for {
			select {
			case <-stop:
				return
			default:
				rec.Flush(context.Background())
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for range goroutines {
		recorders.Add(1)
		go func() {
			defer recorders.Done()
			for range perGoroutine {
				rec.Record(1, store.UsageDelta{Requests: 1, EventsServed: 2})
			}
		}()
	}
	recorders.Wait()
	close(stop)
	flusher.Wait()
	rec.Flush(context.Background()) // drain whatever the last flush missed

	requests, events, _ := sumUsage(spy.rows(1))
	assert.Equal(t, int64(goroutines*perGoroutine), requests,
		"every recorded request must survive concurrent flushing")
	assert.Equal(t, int64(2*goroutines*perGoroutine), events,
		"every recorded event must survive concurrent flushing")
}

// TestUsageMiddlewareCountsOneRequestPerTenant covers the request-counting
// middleware directly: exactly one request per authenticated, tenanted
// request, and nothing for principals that carry no tenant to bill.
func TestUsageMiddlewareCountsOneRequestPerTenant(t *testing.T) {
	type principalHolder struct{ p Principal }

	cases := []struct {
		name         string
		principal    *principalHolder
		wantRequests int64
	}{
		{
			name:         "a tenanted request is counted",
			principal:    &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}},
			wantRequests: 1,
		},
		{
			name:         "an untenanted request is not counted",
			principal:    &principalHolder{Principal{Untenanted: true}},
			wantRequests: 0,
		},
		{
			name:         "a request with no principal is not counted",
			principal:    nil,
			wantRequests: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newUsageSpy()
			srv := newUsageTestServer(spy)
			handler := srv.usageMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			if tc.principal != nil {
				req = req.WithContext(WithPrincipal(req.Context(), tc.principal.p))
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)
			srv.usage.Flush(context.Background())

			requests, _, _ := sumUsage(spy.rows(7))
			assert.Equal(t, tc.wantRequests, requests)
		})
	}

	t.Run("two requests count twice", func(t *testing.T) {
		spy := newUsageSpy()
		srv := newUsageTestServer(spy)
		handler := srv.usageMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		for range 2 {
			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			req = req.WithContext(WithPrincipal(req.Context(),
				Principal{Tenant: store.Tenant{ID: 7}}))
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}
		srv.usage.Flush(context.Background())

		requests, _, _ := sumUsage(spy.rows(7))
		assert.Equal(t, int64(2), requests)
	})
}

// TestRecordEventsServed covers the page-attribution helper, including its
// boundary: a zero or negative page changes nothing, and principals without
// a tenant to bill are skipped.
func TestRecordEventsServed(t *testing.T) {
	type principalHolder struct{ p Principal }

	cases := []struct {
		name       string
		n          int
		principal  *principalHolder
		wantEvents int64
	}{
		{name: "a page of events is attributed", n: 4,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantEvents: 4},
		{name: "a zero-sized page is ignored", n: 0,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantEvents: 0},
		{name: "a negative page is ignored", n: -2,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantEvents: 0},
		{name: "untenanted pages are not attributed", n: 4,
			principal: &principalHolder{Principal{Untenanted: true}}, wantEvents: 0},
		{name: "a page with no principal is not attributed", n: 4,
			principal: nil, wantEvents: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newUsageSpy()
			srv := newUsageTestServer(spy)

			ctx := context.Background()
			if tc.principal != nil {
				ctx = WithPrincipal(ctx, tc.principal.p)
			}
			srv.recordEventsServed(ctx, tc.n)
			srv.usage.Flush(context.Background())

			_, events, _ := sumUsage(spy.rows(7))
			assert.Equal(t, tc.wantEvents, events)
		})
	}
}

// TestRecordStreamTime covers the stream-duration helper. Durations are
// truncated to whole seconds and a sub-second stream books nothing, which
// is the documented accounting granularity — pinning it here means a change
// to rounding has to be a conscious one.
func TestRecordStreamTime(t *testing.T) {
	type principalHolder struct{ p Principal }

	cases := []struct {
		name        string
		duration    time.Duration
		principal   *principalHolder
		wantSeconds int64
	}{
		{name: "a multi-second stream is attributed in whole seconds", duration: 2500 * time.Millisecond,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantSeconds: 2},
		{name: "exactly one second is attributed", duration: 1 * time.Second,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantSeconds: 1},
		{name: "a sub-second stream books nothing", duration: 999 * time.Millisecond,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantSeconds: 0},
		{name: "a zero duration is ignored", duration: 0,
			principal: &principalHolder{Principal{Tenant: store.Tenant{ID: 7}}}, wantSeconds: 0},
		{name: "untenanted streams are not attributed", duration: 5 * time.Second,
			principal: &principalHolder{Principal{Untenanted: true}}, wantSeconds: 0},
		{name: "a stream with no principal is not attributed", duration: 5 * time.Second,
			principal: nil, wantSeconds: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newUsageSpy()
			srv := newUsageTestServer(spy)

			ctx := context.Background()
			if tc.principal != nil {
				ctx = WithPrincipal(ctx, tc.principal.p)
			}
			srv.recordStreamTime(ctx, tc.duration)
			srv.usage.Flush(context.Background())

			_, _, seconds := sumUsage(spy.rows(7))
			assert.Equal(t, tc.wantSeconds, seconds)
		})
	}
}

// TestUsageDayRangeIsBounded pins the documented day-range bound on the
// reporting endpoints: absent means the 30-day default, [1, 365] is
// accepted, and anything else is a 400 that never reaches the store.
func TestUsageDayRangeIsBounded(t *testing.T) {
	router, spy, _, keyA, _, keyAdmin := newUsageRouter(t)

	cases := []struct {
		name       string
		days       string
		wantStatus int
		wantDays   int // -1 asserts the store was never asked
	}{
		{name: "absent defaults to a month", days: "", wantStatus: http.StatusOK, wantDays: 30},
		{name: "one day is accepted", days: "1", wantStatus: http.StatusOK, wantDays: 1},
		{name: "the documented maximum is accepted", days: "365", wantStatus: http.StatusOK, wantDays: 365},
		{name: "zero is refused", days: "0", wantStatus: http.StatusBadRequest, wantDays: -1},
		{name: "negative is refused", days: "-1", wantStatus: http.StatusBadRequest, wantDays: -1},
		{name: "past the maximum is refused", days: "366", wantStatus: http.StatusBadRequest, wantDays: -1},
		{name: "non-numeric is refused", days: "week", wantStatus: http.StatusBadRequest, wantDays: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(spy.daysSeen())
			path := "/tenant/usage"
			if tc.days != "" {
				path += "?days=" + tc.days
			}

			rec := usageGet(t, router, keyA, path)

			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			seen := spy.daysSeen()
			if tc.wantDays < 0 {
				assert.Len(t, seen, before, "a refused range must not reach the store")
				return
			}
			require.Len(t, seen, before+1)
			assert.Equal(t, tc.wantDays, seen[len(seen)-1],
				"the store must be asked for exactly the range the caller named")
		})
	}

	t.Run("the admin endpoint is bounded the same way", func(t *testing.T) {
		rec := usageGet(t, router, keyAdmin, "/admin/tenants/1/usage?days=366")
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		rec = usageGet(t, router, keyAdmin, "/admin/tenants/1/usage?days=7")
		require.Equal(t, http.StatusOK, rec.Code)
		seen := spy.daysSeen()
		require.NotEmpty(t, seen)
		assert.Equal(t, 7, seen[len(seen)-1])
	})
}

// A tenant with no recorded activity before this request must get a 200
// with a zeroed usage row, not an error — quiet tenants still call the
// reporting endpoint, and surfacing a store miss as a failure would make
// every first-day tenant look broken. (The self-service endpoint flushes
// before reading, so the GET itself books one request; the row must reflect
// exactly that and nothing else.)
func TestUsageForQuietTenantReturnsZeroes(t *testing.T) {
	router, _, _, _, keyQuiet, _ := newUsageRouter(t)

	rec := usageGet(t, router, keyQuiet, "/tenant/usage")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp struct {
		Usage []store.TenantUsage `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Usage, 1, "the flush before reading books this request itself")

	requests, events, seconds := sumUsage(resp.Usage)
	assert.Equal(t, int64(1), requests, "only the reporting request is counted")
	assert.Equal(t, int64(0), events, "no events have been served to this tenant")
	assert.Equal(t, int64(0), seconds, "no streams have been attributed to this tenant")
	assert.NotContains(t, rec.Body.String(), "\"error\"",
		"no activity is an answer, not an error")
}

// The self-service endpoint flushes before reading, so a tenant checking
// its usage sees the requests it just made rather than a figure up to one
// interval stale.
func TestOwnUsageFlushesPendingFirst(t *testing.T) {
	router, spy, srv, keyA, _, _ := newUsageRouter(t)

	srv.usage.Record(1, store.UsageDelta{Requests: 2, EventsServed: 4})

	rec := usageGet(t, router, keyA, "/tenant/usage")
	require.Equal(t, http.StatusOK, rec.Code)

	rows := spy.rows(1)
	require.NotEmpty(t, rows, "reading own usage must flush the pending counters first")
	requests, _, _ := sumUsage(rows)
	// Two recorded requests plus this one: the middleware books the current
	// GET before the handler flushes, which is exactly what "sees the
	// requests it just made" promises.
	assert.Equal(t, int64(3), requests)
}
