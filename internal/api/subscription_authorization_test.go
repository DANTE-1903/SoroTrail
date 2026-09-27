package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// Direct coverage for authorizeSubscriptionFilters and its companions in
// handlers_subscription.go. The end-to-end leak tests (TestSubscriptionCreationIsScoped
// and friends) prove the router wiring; these tests prove the helper's own
// decision table, so a regression in the helper fails here with a message
// that names the rule, not just a status code.

// subPrincipal builds a request carrying p, the same shape the authenticate
// middleware injects before any handler runs.
func subPrincipal(p Principal) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/subscriptions", nil)
	return r.WithContext(WithPrincipal(r.Context(), p))
}

func TestAuthorizeSubscriptionFilters(t *testing.T) {
	granted := store.NewScope([]string{contractA})

	cases := []struct {
		name      string
		principal Principal
		present   bool // whether the context carries a principal at all
		filter    store.SubscriptionFilter
		wantErr   string // empty means the filter must be accepted
	}{
		{
			name:      "a granted contract is accepted",
			principal: Principal{Tenant: store.Tenant{ID: 1}, Scope: granted},
			present:   true,
			filter:    store.SubscriptionFilter{ContractID: contractA},
		},
		{
			name:      "a contract the caller cannot read is rejected",
			principal: Principal{Tenant: store.Tenant{ID: 1}, Scope: granted},
			present:   true,
			filter:    store.SubscriptionFilter{ContractID: contractB},
			wantErr:   "not granted",
		},
		{
			name:      "an unscoped filter is rejected for a scoped tenant",
			principal: Principal{Tenant: store.Tenant{ID: 1}, Scope: granted},
			present:   true,
			filter:    store.SubscriptionFilter{},
			wantErr:   "filters.contract_id is required",
		},
		{
			name:      "a wildcard tenant may subscribe unfiltered",
			principal: Principal{Tenant: store.Tenant{ID: 5}, Scope: store.WildcardScope()},
			present:   true,
			filter:    store.SubscriptionFilter{},
		},
		{
			name:      "a wildcard tenant may subscribe to any contract",
			principal: Principal{Tenant: store.Tenant{ID: 5}, Scope: store.WildcardScope()},
			present:   true,
			filter:    store.SubscriptionFilter{ContractID: contractB},
		},
		{
			name:      "an untenanted principal bypasses the filter requirement even with an empty scope",
			principal: Principal{Untenanted: true},
			present:   true,
			filter:    store.SubscriptionFilter{},
		},
		{
			name:      "an admin without wildcard is still constrained to its grants",
			principal: Principal{Tenant: store.Tenant{ID: 4, Admin: true}, Scope: store.NewScope(nil)},
			present:   true,
			filter:    store.SubscriptionFilter{},
			wantErr:   "filters.contract_id is required",
		},
		{
			name:      "an admin may subscribe to a contract it holds",
			principal: Principal{Tenant: store.Tenant{ID: 4, Admin: true}, Scope: granted},
			present:   true,
			filter:    store.SubscriptionFilter{ContractID: contractA},
		},
		{
			name:      "a missing principal is rejected outright",
			present:   false,
			principal: Principal{},
			filter:    store.SubscriptionFilter{ContractID: contractA},
			wantErr:   "unauthenticated",
		},
		{
			name:      "an empty contract id is not mistaken for a grant even on a wildcard scope check order",
			principal: Principal{Tenant: store.Tenant{ID: 1}, Scope: granted},
			present:   true,
			filter:    store.SubscriptionFilter{Type: "contract"},
			wantErr:   "filters.contract_id is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := subPrincipal(tc.principal)
			if !tc.present {
				r = httptest.NewRequest(http.MethodPost, "/subscriptions", nil)
			}

			err := authorizeSubscriptionFilters(r, tc.filter)

			if tc.wantErr == "" {
				assert.NoError(t, err, "the filter describes only what the caller may read")
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// The rejection for a foreign contract must name the offending filter's
// value: an operator staring at a 403 has to know which contract_id was
// refused without re-deriving it from the request body.
func TestAuthorizeSubscriptionFilters_NamesTheOffendingContract(t *testing.T) {
	r := subPrincipal(Principal{Tenant: store.Tenant{ID: 1}, Scope: store.NewScope([]string{contractA})})

	err := authorizeSubscriptionFilters(r, store.SubscriptionFilter{ContractID: contractB})

	require.Error(t, err)
	assert.Contains(t, err.Error(), contractB)
}

// The unscoped rejection must point at the JSON field that has to be set,
// since "your subscription is too broad" alone leaves the caller guessing
// which part of the filter to fix.
func TestAuthorizeSubscriptionFilters_UnscopedMessageNamesTheFilterField(t *testing.T) {
	r := subPrincipal(Principal{Tenant: store.Tenant{ID: 1}, Scope: store.NewScope([]string{contractA})})

	err := authorizeSubscriptionFilters(r, store.SubscriptionFilter{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "filters.contract_id")
}

func TestWriteFilterError(t *testing.T) {
	t.Run("a forbidden contract is reported as 403 naming the contract", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeFilterError(rec, errForbiddenContract{contractID: contractB})

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), contractB)
		assert.Contains(t, rec.Body.String(), "not granted")
	})

	t.Run("any other filter failure is a 400 carrying the message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeFilterError(rec, assert.AnError)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), assert.AnError.Error())
	})
}

func TestParseSubscriptionID(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantID  int64
		wantErr bool
	}{
		{name: "a positive integer parses", raw: "7", wantID: 7},
		{name: "a large id parses", raw: "9007199254740992", wantID: 9007199254740992},
		{name: "zero is not an id", raw: "0", wantErr: true},
		{name: "negative is not an id", raw: "-3", wantErr: true},
		{name: "non-numeric is not an id", raw: "abc", wantErr: true},
		{name: "empty is not an id", raw: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.raw)
			r := httptest.NewRequest(http.MethodGet, "/subscriptions/"+tc.raw, nil).
				WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

			id, err := parseSubscriptionID(r)

			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// End to end through the router: the create path must answer 400 with a
// body naming filters.contract_id when a scoped tenant omits it, and 403
// when it names a contract outside its grants. The 400-on-invalid-contract-id
// expectation from the issue is satisfied by the missing-filter branch — the
// subscription path deliberately does no shape validation of its own, so a
// syntactically malformed (but non-empty) contract id is treated like any
// other ungranted contract and rejected with 403.
func TestSubscriptionFilterAuthorization_EndToEnd(t *testing.T) {
	f, _ := newSubFixture(t)

	t.Run("missing contract filter is a 400 naming the field", func(t *testing.T) {
		rec := f.post(t, f.keyA, "/subscriptions", subBody(""))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, bodyString(t, rec), "filters.contract_id")
	})

	t.Run("ungranted contract is a 403 naming the contract", func(t *testing.T) {
		rec := f.post(t, f.keyA, "/subscriptions", subBody(contractC))
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, bodyString(t, rec), contractC)
	})

	t.Run("granted contract is a 201", func(t *testing.T) {
		rec := f.post(t, f.keyA, "/subscriptions", subBody(contractA))
		assert.Equal(t, http.StatusCreated, rec.Code, bodyString(t, rec))
	})
}
