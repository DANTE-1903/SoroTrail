package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// The authentication helpers are the API's authorization boundary: they
// decide who may reach a gated endpoint, and a regression here is silent
// until someone reads data they were never granted. These tests exercise
// each helper directly rather than only through the router, so a failure
// names the helper that broke instead of a status code somewhere.
//
// The fail-closed direction is asserted at least as carefully as success:
// an absent key, an unknown key, a disabled tenant, and an unconfigured
// API_KEY must all deny, and must deny without leaking why.

// authRecorder is a terminal handler that records whether it ran and the
// request it was handed, so a test can assert both "the gate allowed the
// call through" and "the principal reached the handler".
type authRecorder struct {
	called bool
	req    *http.Request
}

func (h *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.req = r
	w.WriteHeader(http.StatusOK)
}

// runAuthMiddleware drives one middleware chain against req and reports the
// response alongside the terminal handler.
func runAuthMiddleware(mw func(http.Handler) http.Handler, req *http.Request) (*httptest.ResponseRecorder, *authRecorder) {
	h := &authRecorder{}
	rec := httptest.NewRecorder()
	mw(h).ServeHTTP(rec, req)
	return rec, h
}

// authTenants builds the three-tenant deployment the authentication tests
// share: an ordinary tenant, an admin tenant, and a suspended tenant. It
// reuses the package's fakeTenants so the tests stay on the same fakes as
// the rest of the suite.
func authTenants(t *testing.T) (ts *fakeTenants, keyA, keyAdmin, keyDisabled string) {
	t.Helper()
	ts = newFakeTenants()
	keyA = ts.addTenant(t, store.Tenant{ID: 1, Name: "a", Enabled: true}, contractA)
	keyAdmin = ts.addTenant(t, store.Tenant{ID: 2, Name: "admin", Enabled: true, Admin: true}, contractA)
	keyDisabled = ts.addTenant(t, store.Tenant{ID: 3, Name: "suspended", Enabled: false}, contractA)
	return ts, keyA, keyAdmin, keyDisabled
}

// newMultiTenantAuthServer builds a Server in multi-tenant mode, which is
// the only mode in which authenticate consults credentials at all. The
// package-level caching flag is process-wide, so it is reset on cleanup.
func newMultiTenantAuthServer(t *testing.T, ts store.TenantStore) *Server {
	t.Helper()
	SetTenantScopedCaching(false)
	t.Cleanup(func() { SetTenantScopedCaching(false) })
	return New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "").
		WithMultiTenancy(ts, MultiTenantOptions{MaxWatchedContracts: 10})
}

// decodeEnvelope asserts the body is the shared {"error": ...} envelope and
// returns it, so every rejection is checked against one shape.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var env errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env),
		"body must be the shared error envelope, got %q", rec.Body.String())
	return env
}

// errTenants lets a test force a persistence failure at a chosen point in
// authenticate, so the 500 branches are covered rather than assumed. It
// embeds the real fake so only the injected method changes.
type errTenants struct {
	*fakeTenants
	lookupErr error
	scopeErr  error
}

func (e *errTenants) LookupAPIKey(ctx context.Context, prefix string) (store.APIKey, []byte, store.Tenant, error) {
	if e.lookupErr != nil {
		return store.APIKey{}, nil, store.Tenant{}, e.lookupErr
	}
	return e.fakeTenants.LookupAPIKey(ctx, prefix)
}

func (e *errTenants) ScopeForTenant(ctx context.Context, t store.Tenant) (store.Scope, error) {
	if e.scopeErr != nil {
		return store.Scope{}, e.scopeErr
	}
	return e.fakeTenants.ScopeForTenant(ctx, t)
}

// TestAPIKeyAuthHelper covers the operator API_KEY gate on the writes.
// Its defining property is fail-closed: no configured key means no writes.
func TestAPIKeyAuthHelper(t *testing.T) {
	t.Run("a matching header is accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/events", nil)
		req.Header.Set("X-API-Key", "operator-secret")
		rec, h := runAuthMiddleware(apiKeyAuth("operator-secret"), req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, h.called, "a correct key must reach the handler")
	})

	t.Run("a missing or wrong header is rejected", func(t *testing.T) {
		cases := []struct {
			name   string
			header string
		}{
			{"absent header", ""},
			{"empty header", " "},
			{"wrong key", "not-the-key"},
			{"a prefix of the key", "operator-secre"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodDelete, "/events", nil)
				if tc.header != "" {
					req.Header.Set("X-API-Key", tc.header)
				}
				rec, h := runAuthMiddleware(apiKeyAuth("operator-secret"), req)

				require.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.False(t, h.called, "a rejected key must never reach the handler")
				assert.Equal(t, errAuthFailed.Error(), decodeEnvelope(t, rec).Error)
				assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			})
		}
	})

	t.Run("an empty configured API_KEY fails closed for every caller", func(t *testing.T) {
		// The dangerous regression is treating "" as "no auth required":
		// a request carrying any (or no) header would then reach the write
		// handler. Every caller must instead get 503 naming the env var.
		cases := []struct {
			name   string
			header string
		}{
			{"no header", ""},
			{"an arbitrary header", "anything"},
			{"an empty-string header", " "},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodDelete, "/events", nil)
				if tc.header != "" {
					req.Header.Set("X-API-Key", tc.header)
				}
				rec, h := runAuthMiddleware(apiKeyAuth(""), req)

				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
				assert.False(t, h.called, "writes must be closed when no key is configured")
				assert.Equal(t, errAuthNotConfigured.Error(), decodeEnvelope(t, rec).Error)
			})
		}
	})
}

// TestAuthenticateHelper covers the credential-presentation path: which
// requests authenticate, which are rejected, and what principal a successful
// request carries. The principal's scope is the value every downstream read
// depends on, so it is asserted directly.
func TestAuthenticateHelper(t *testing.T) {
	tenants, keyA, _, keyDisabled := authTenants(t)

	unknown, _, _, err := GenerateAPIKey()
	require.NoError(t, err)
	parts := strings.Split(keyA, "_")
	require.Len(t, parts, 3)
	// A well-formed key that reuses a real, indexed prefix but a secret
	// nobody minted. Knowing the prefix must not be enough.
	forged := parts[0] + "_" + parts[1] + "_" + strings.Repeat("A", keySecretLen)

	build := func(bearer, xkey string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if xkey != "" {
			req.Header.Set("X-API-Key", xkey)
		}
		return req
	}

	cases := []struct {
		name         string
		bearer, xkey string
		wantStatus   int
		wantCalled   bool
		wantError    string
		wantWWWAuth  bool
		wantTenantID int64
		wantKeyID    int64
		wantScope    []string
	}{
		{
			name: "a valid bearer key authenticates", bearer: keyA,
			wantStatus: http.StatusOK, wantCalled: true,
			wantTenantID: 1, wantKeyID: 100, wantScope: []string{contractA},
		},
		{
			name: "a valid X-API-Key authenticates", xkey: keyA,
			wantStatus: http.StatusOK, wantCalled: true,
			wantTenantID: 1, wantKeyID: 100, wantScope: []string{contractA},
		},
		{
			name: "bearer wins when both credentials are present", bearer: keyA, xkey: "some-other-key",
			wantStatus: http.StatusOK, wantCalled: true,
			wantTenantID: 1, wantKeyID: 100, wantScope: []string{contractA},
		},
		{
			// The precedence runs the other way too: a bad Bearer must not
			// be rescued by a good X-API-Key.
			name: "a wrong bearer is not rescued by a valid X-API-Key", bearer: forged, xkey: keyA,
			wantStatus: http.StatusUnauthorized, wantError: "unknown or revoked API key", wantWWWAuth: true,
		},
		{
			name:       "an absent credential is rejected",
			wantStatus: http.StatusUnauthorized, wantError: "missing API key", wantWWWAuth: true,
		},
		{
			name: "a malformed credential is rejected", bearer: "not-a-key",
			wantStatus: http.StatusUnauthorized, wantError: "malformed API key", wantWWWAuth: true,
		},
		{
			name: "an unknown but well-formed key is rejected", bearer: unknown,
			wantStatus: http.StatusUnauthorized, wantError: "unknown or revoked API key", wantWWWAuth: true,
		},
		{
			name: "the right prefix with the wrong secret is rejected", bearer: forged,
			wantStatus: http.StatusUnauthorized, wantError: "unknown or revoked API key", wantWWWAuth: true,
		},
		{
			name: "a disabled tenant is refused", bearer: keyDisabled,
			wantStatus: http.StatusForbidden, wantError: "tenant is disabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMultiTenantAuthServer(t, tenants)
			rec, h := runAuthMiddleware(srv.authenticate, build(tc.bearer, tc.xkey))

			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, tc.wantCalled, h.called)

			if !tc.wantCalled {
				assert.Equal(t, tc.wantError, decodeEnvelope(t, rec).Error)
				if tc.wantWWWAuth {
					assert.Equal(t, `Bearer realm="sorotrail"`, rec.Header().Get("WWW-Authenticate"),
						"RFC 7235 §4.1 requires a challenge on 401")
				} else {
					assert.Empty(t, rec.Header().Get("WWW-Authenticate"),
						"403 is not an authentication challenge")
				}
				assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"),
					"a rejection body must not be cached")
				return
			}

			p, ok := PrincipalFrom(h.req.Context())
			require.True(t, ok, "an authenticated request must carry a principal")
			assert.False(t, p.Untenanted)
			assert.Equal(t, tc.wantTenantID, p.Tenant.ID)
			assert.Equal(t, tc.wantKeyID, p.KeyID, "the key that authenticated must be recorded")
			assert.False(t, p.Scope.IsWildcard())
			assert.Equal(t, tc.wantScope, p.Scope.Contracts(),
				"the caller's scope must be resolved at authentication time")
		})
	}
}

// TestAuthenticatePublicPathsBypassCredentials pins the one deliberate
// exception: orchestrator probes carry no credential but expose no tenant
// data, so gating them would leave a multi-tenant pod failing readiness.
func TestAuthenticatePublicPathsBypassCredentials(t *testing.T) {
	tenants, _, _, _ := authTenants(t)
	srv := newMultiTenantAuthServer(t, tenants)

	for _, path := range []string{"/health", "/livez", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil) // no credential
			rec, h := runAuthMiddleware(srv.authenticate, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.True(t, h.called, "%s must stay reachable without a key", path)
			_, ok := PrincipalFrom(h.req.Context())
			assert.False(t, ok, "a public path must not be given a principal it did not earn")
		})
	}

	t.Run("a non-public path still requires a credential", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		rec, h := runAuthMiddleware(srv.authenticate, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.False(t, h.called)
	})
}

// TestAuthenticateSingleTenantMode covers the default deployment: no
// credential is required, and every request runs as the implicit wildcard
// principal with Untenanted set, so handlers can tell the two modes apart.
func TestAuthenticateSingleTenantMode(t *testing.T) {
	SetTenantScopedCaching(false)
	t.Cleanup(func() { SetTenantScopedCaching(false) })
	srv := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")

	cases := []struct {
		name   string
		header func(*http.Request)
	}{
		{"no credential", func(*http.Request) {}},
		{"a bogus credential is ignored, not rejected", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer garbage")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			tc.header(req)
			rec, h := runAuthMiddleware(srv.authenticate, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.True(t, h.called)

			p, ok := PrincipalFrom(h.req.Context())
			require.True(t, ok, "single-tenant mode must always install a principal")
			assert.True(t, p.Untenanted)
			assert.True(t, p.Scope.IsWildcard())
			assert.Zero(t, p.Tenant.ID)
		})
	}
}

// TestAuthenticateFailsClosedOnStoreErrors covers the two persistence
// failures on the authentication path. Both must deny with 500 rather than
// let a request through with a missing or partial principal.
func TestAuthenticateFailsClosedOnStoreErrors(t *testing.T) {
	cases := []struct {
		name     string
		inject   func(*errTenants)
		wantCode int
	}{
		{
			name:     "a key-lookup failure denies",
			inject:   func(e *errTenants) { e.lookupErr = errors.New("database down") },
			wantCode: http.StatusInternalServerError,
		},
		{
			name:     "a scope-resolution failure denies",
			inject:   func(e *errTenants) { e.scopeErr = errors.New("database down") },
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, keyA, _, _ := authTenants(t)
			ts := &errTenants{fakeTenants: base}
			tc.inject(ts)

			srv := newMultiTenantAuthServer(t, ts)
			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			req.Header.Set("Authorization", "Bearer "+keyA)

			rec, h := runAuthMiddleware(srv.authenticate, req)
			assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
			assert.False(t, h.called)
			assert.NotContains(t, rec.Body.String(), "database down",
				"internal errors must not be echoed to the caller")
		})
	}
}

// TestRequireAdminHelper covers the /admin/* gate independently of the
// router, including the single-tenant answer (404, not 403: there are no
// tenants to administer, so the route reads as absent).
func TestRequireAdminHelper(t *testing.T) {
	admin := Principal{Tenant: store.Tenant{ID: 2, Admin: true}, Scope: store.WildcardScope()}
	nonAdmin := Principal{Tenant: store.Tenant{ID: 1}, Scope: store.NewScope([]string{contractA})}
	untenanted := Principal{Untenanted: true, Scope: store.WildcardScope()}

	cases := []struct {
		name       string
		principal  *Principal
		wantStatus int
		wantCalled bool
		wantError  string
	}{
		{
			name: "no principal is treated as not-multi-tenant", wantStatus: http.StatusNotFound,
			wantError: "admin API is available only when MULTI_TENANT=true",
		},
		{
			name: "the single-tenant principal is refused", principal: &untenanted,
			wantStatus: http.StatusNotFound,
			wantError:  "admin API is available only when MULTI_TENANT=true",
		},
		{
			name: "a non-admin tenant is forbidden", principal: &nonAdmin,
			wantStatus: http.StatusForbidden, wantError: "admin privileges required",
		},
		{
			name: "an admin tenant is allowed", principal: &admin,
			wantStatus: http.StatusOK, wantCalled: true,
		},
	}

	srv := newMultiTenantAuthServer(t, newFakeTenants())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
			if tc.principal != nil {
				req = req.WithContext(WithPrincipal(req.Context(), *tc.principal))
			}
			rec, h := runAuthMiddleware(srv.requireAdmin, req)

			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, tc.wantCalled, h.called)
			if !tc.wantCalled {
				assert.Equal(t, tc.wantError, decodeEnvelope(t, rec).Error)
			}
		})
	}

	t.Run("an allowed request keeps its principal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
		req = req.WithContext(WithPrincipal(req.Context(), admin))
		_, h := runAuthMiddleware(srv.requireAdmin, req)

		p, ok := PrincipalFrom(h.req.Context())
		require.True(t, ok)
		assert.Equal(t, int64(2), p.Tenant.ID)
		assert.True(t, p.Tenant.Admin)
	})
}

// TestRequireTenantHelper covers the /tenant/* gate. Unlike requireAdmin it
// has no admin check: any real tenant may see its own identity and usage.
func TestRequireTenantHelper(t *testing.T) {
	tenant := Principal{Tenant: store.Tenant{ID: 7, Name: "seven"}, Scope: store.NewScope([]string{contractA})}
	untenanted := Principal{Untenanted: true, Scope: store.WildcardScope()}

	cases := []struct {
		name       string
		principal  *Principal
		wantStatus int
		wantCalled bool
		wantError  string
	}{
		{
			name: "no principal is treated as not-multi-tenant", wantStatus: http.StatusNotFound,
			wantError: "tenant API is available only when MULTI_TENANT=true",
		},
		{
			name: "the single-tenant principal is refused", principal: &untenanted,
			wantStatus: http.StatusNotFound, wantError: "tenant API is available only when MULTI_TENANT=true",
		},
		{
			name: "a real tenant is allowed", principal: &tenant,
			wantStatus: http.StatusOK, wantCalled: true,
		},
	}

	srv := newMultiTenantAuthServer(t, newFakeTenants())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/tenant", nil)
			if tc.principal != nil {
				req = req.WithContext(WithPrincipal(req.Context(), *tc.principal))
			}
			rec, h := runAuthMiddleware(srv.requireTenant, req)

			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, tc.wantCalled, h.called)
			if !tc.wantCalled {
				assert.Equal(t, tc.wantError, decodeEnvelope(t, rec).Error)
			}
		})
	}
}

// TestTenantRouteCarriesAuthenticatedScope chains the real middleware the
// router installs (authenticate → requireTenant) and asserts the handler
// sees the caller's resolved scope. The scope is what every tenant read
// depends on; a gate that admits the request but drops the scope would be
// broken in the quietest possible way.
func TestTenantRouteCarriesAuthenticatedScope(t *testing.T) {
	tenants, keyA, _, _ := authTenants(t)
	srv := newMultiTenantAuthServer(t, tenants)

	h := &authRecorder{}
	chained := srv.authenticate(srv.requireTenant(h))

	req := httptest.NewRequest(http.MethodGet, "/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.True(t, h.called)

	p, ok := PrincipalFrom(h.req.Context())
	require.True(t, ok)
	assert.Equal(t, int64(1), p.Tenant.ID)
	assert.Equal(t, []string{contractA}, scopeFrom(h.req.Context()).Contracts())
	assert.False(t, scopeFrom(h.req.Context()).IsWildcard())
}

// TestWriteUnauthorizedHelper pins the shape of every 401 the API emits:
// the shared error envelope, a challenge header, no caching, and no detail
// beyond the message it was given.
func TestWriteUnauthorizedHelper(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUnauthorized(rec, "missing API key")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `Bearer realm="sorotrail"`, rec.Header().Get("WWW-Authenticate"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	// Exactly one key, and it is the error. An extra field would be a
	// place for an internal reason to leak; a missing one would break
	// clients that decode the shared envelope.
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1, "the envelope must carry only the error field")
	assert.Equal(t, "missing API key", body["error"])

	var env errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "missing API key", env.Error)
}
