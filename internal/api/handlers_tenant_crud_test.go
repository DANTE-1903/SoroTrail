package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/rpc"
	"github.com/sorotrail/sorotrail/internal/store"
)

// Direct coverage for the tenant CRUD, grant and key helpers in
// handlers_tenant.go. The cross-tenant leak matrix in tenant_test.go proves
// the boundary; this file proves the helper-level mechanics the issues ask
// for: create stores and returns the tenant, grants become visible and
// revocations take effect on the next read, keys are returned exactly once
// and stored only as a digest, and unknown tenants answer 404.

// crudpTenants extends fakeTenants with the write paths the CRUD and key
// helpers exercise. It embeds the fake from tenant_test.go, so the read
// methods already exist and only the mutating ones are filled in here.
type crudpTenants struct {
	*fakeTenants

	createdTenants map[int64]store.Tenant
	updatedTenants map[int64]store.Tenant
	deletedTenants map[int64]bool
	createdKeys    map[int64][]store.TenantAPIKey // by tenant, in mint order
	keyByIDs       map[int64]store.TenantAPIKey   // every minted key, sans secret
	revokedKeys    map[int64]bool                 // by key ID
	digests        map[int64][]byte               // stored digest by key ID

	nextTenantID int64
	nextKeyID    int64
}

func newCRUDPTenants(f *fakeTenants) *crudpTenants {
	return &crudpTenants{
		fakeTenants:    f,
		createdTenants: map[int64]store.Tenant{},
		updatedTenants: map[int64]store.Tenant{},
		deletedTenants: map[int64]bool{},
		createdKeys:    map[int64][]store.TenantAPIKey{},
		keyByIDs:       map[int64]store.TenantAPIKey{},
		revokedKeys:    map[int64]bool{},
		digests:        map[int64][]byte{},
		nextTenantID:   100, // keep clear of the fixture's tenants 1..5
		nextKeyID:      1000,
	}
}

func (c *crudpTenants) CreateTenant(_ context.Context, t store.Tenant) (store.Tenant, error) {
	c.nextTenantID++
	t.ID = c.nextTenantID
	c.tenants[t.ID] = t
	c.createdTenants[t.ID] = t
	return t, nil
}

func (c *crudpTenants) UpdateTenant(_ context.Context, t store.Tenant) (store.Tenant, error) {
	c.updatedTenants[t.ID] = t
	c.tenants[t.ID] = t
	return t, nil
}

func (c *crudpTenants) DeleteTenant(_ context.Context, id int64) error {
	c.deletedTenants[id] = true
	delete(c.tenants, id)
	return nil
}

func (c *crudpTenants) GrantContract(_ context.Context, tenantID int64, contractID string) error {
	c.grants[tenantID] = append(c.grants[tenantID], contractID)
	return nil
}

func (c *crudpTenants) RevokeContract(_ context.Context, tenantID int64, contractID string) error {
	list := c.grants[tenantID]
	out := list[:0]
	for _, g := range list {
		if g != contractID {
			out = append(out, g)
		}
	}
	c.grants[tenantID] = out
	return nil
}

func (c *crudpTenants) CreateTenantAPIKey(_ context.Context, tenantID int64, name, prefix string, digest []byte) (store.TenantAPIKey, error) {
	c.nextKeyID++
	key := store.TenantAPIKey{
		ID:       c.nextKeyID,
		TenantID: tenantID,
		Name:     name,
		Prefix:   prefix,
	}
	c.keys[prefix] = keyRecord{id: key.ID, tenantID: tenantID, digest: digest}
	c.createdKeys[tenantID] = append(c.createdKeys[tenantID], key)
	c.keyByIDs[key.ID] = key
	c.digests[key.ID] = digest
	return key, nil
}

// ListTenantAPIKeys overrides the embedded fake so listed records carry the
// name and prefix the real store returns — the prefix is what operators use
// to identify a key after creation day.
func (c *crudpTenants) ListTenantAPIKeys(_ context.Context, tenantID int64) ([]store.TenantAPIKey, error) {
	out := []store.TenantAPIKey{}
	for _, key := range c.keyByIDs {
		if key.TenantID == tenantID {
			out = append(out, key)
		}
	}
	return out, nil
}

func (c *crudpTenants) RevokeTenantAPIKey(_ context.Context, id int64) error {
	for prefix, rec := range c.keys {
		if rec.id == id {
			c.revokedKeys[id] = true
			delete(c.keys, prefix)
			return nil
		}
	}
	return store.ErrNotFound
}

// crudpFixture is the admin-authenticated router plus the recording fake.
type crudpFixture struct {
	srv     http.Handler
	tenants *crudpTenants

	adminPlaintext string
}

func newCRUDPFixture(t *testing.T) *crudpFixture {
	t.Helper()
	SetTenantScopedCaching(false)
	t.Cleanup(func() { SetTenantScopedCaching(false) })

	base := newFakeTenants()
	crud := newCRUDPTenants(base)
	// Seed the fixture's admin tenant directly so /admin/* is reachable.
	base.tenants[4] = store.Tenant{ID: 4, Name: "admin", Enabled: true, Admin: true}
	base.grants[4] = nil
	plaintext, prefix, digest, err := GenerateAPIKey()
	require.NoError(t, err)
	base.keys[prefix] = keyRecord{id: 400, tenantID: 4, digest: digest}

	server := New(&scopedStore{}, &stubRPC{health: rpc.Health{Status: "healthy"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test-key").
		WithMultiTenancy(crud, MultiTenantOptions{})
	// decodeJSON wraps the body in MaxBytesReader with this limit verbatim,
	// so an unset limit (0) would reject every JSON body as oversized.
	server.SetHTTPRequestBodyLimit(1 << 20)
	return &crudpFixture{srv: server.Router(), tenants: crud, adminPlaintext: plaintext}
}

func (f *crudpFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+f.adminPlaintext)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func TestTenantCRUDHelpers(t *testing.T) {
	f := newCRUDPFixture(t)

	t.Run("creating a tenant returns its id and stores it", func(t *testing.T) {
		rec := f.do(t, http.MethodPost, "/admin/tenants", `{"name":"acme"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var created store.Tenant
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.NotZero(t, created.ID, "the response carries the new id")
		assert.Equal(t, "acme", created.Name)
		assert.True(t, created.Enabled, "tenants start enabled")

		stored, ok := f.tenants.createdTenants[created.ID]
		require.True(t, ok, "the tenant must be stored, not just echoed")
		assert.Equal(t, "acme", stored.Name)
	})

	t.Run("create validates quota shape", func(t *testing.T) {
		rec := f.do(t, http.MethodPost, "/admin/tenants",
			`{"name":"bad","rate_limit_rps":-1}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "rate_limit_rps")
	})

	t.Run("create rejects a missing name", func(t *testing.T) {
		rec := f.do(t, http.MethodPost, "/admin/tenants", `{}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("update patches only supplied fields", func(t *testing.T) {
		created := f.makeTenant(t, "acme")
		rec := f.do(t, http.MethodPatch, fmt.Sprintf("/admin/tenants/%d", created.ID),
			`{"name":"renamed"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		updated, ok := f.tenants.updatedTenants[created.ID]
		require.True(t, ok, "the update must reach the store")
		assert.Equal(t, "renamed", updated.Name)
		assert.True(t, updated.Enabled, "PATCH must not clobber the stored enabled flag")
	})

	t.Run("deleting a tenant takes effect on the next read", func(t *testing.T) {
		created := f.makeTenant(t, "doomed")
		path := fmt.Sprintf("/admin/tenants/%d", created.ID)
		rec := f.do(t, http.MethodDelete, path, "")
		require.Equal(t, http.StatusNoContent, rec.Code)
		assert.True(t, f.tenants.deletedTenants[created.ID],
			"the delete must reach the store, not merely stop answering")

		rec = f.do(t, http.MethodGet, path, "")
		assert.Equal(t, http.StatusNotFound, rec.Code,
			"a deleted tenant must not be readable")
	})

	t.Run("operations on an unknown tenant return 404", func(t *testing.T) {
		for _, tc := range []struct {
			method, path string
		}{
			{http.MethodGet, "/admin/tenants/987654"},
			{http.MethodPatch, "/admin/tenants/987654"},
			{http.MethodGet, "/admin/tenants/987654/grants"},
			{http.MethodPost, "/admin/tenants/987654/grants"},
			{http.MethodGet, "/admin/tenants/987654/keys"},
			{http.MethodPost, "/admin/tenants/987654/keys"},
			{http.MethodGet, "/admin/tenants/987654/usage"},
		} {
			// PATCH and POST carry a valid JSON body so the handler reaches
			// the tenant lookup rather than failing earlier on decoding.
			body := "{}"
			if tc.method == http.MethodPost {
				body = `{"contract_id":"` + contractA + `"}`
				if strings.HasSuffix(tc.path, "/keys") {
					body = `{"name":"k"}`
				}
			}
			rec := f.do(t, tc.method, tc.path, body)
			assert.Equal(t, http.StatusNotFound, rec.Code,
				"%s %s must 404 for an unknown tenant (body: %s)", tc.method, tc.path, rec.Body.String())
		}
	})

	t.Run("a tenant cannot delete itself", func(t *testing.T) {
		rec := f.do(t, http.MethodDelete, "/admin/tenants/4", "")
		assert.Equal(t, http.StatusConflict, rec.Code)
	})
}

func TestGrantHelpers(t *testing.T) {
	f := newCRUDPFixture(t)

	t.Run("granting a contract makes it visible to that tenant and no other", func(t *testing.T) {
		created := f.makeTenant(t, "g1")
		other := f.makeTenant(t, "g2")

		rec := f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/grants", created.ID),
			`{"contract_id":"`+contractA+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), contractA, "the grant list is echoed back")

		// The grant is visible to the target tenant...
		rec = f.do(t, http.MethodGet, fmt.Sprintf("/admin/tenants/%d/grants", created.ID), "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), contractA)

		// ...and invisible to every other tenant.
		rec = f.do(t, http.MethodGet, fmt.Sprintf("/admin/tenants/%d/grants", other.ID), "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), contractA)
	})

	t.Run("an invalid contract id is rejected with 400", func(t *testing.T) {
		created := f.makeTenant(t, "g3")
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/grants", created.ID),
			`{"contract_id":"not-a-contract-id"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid contract_id")
	})

	t.Run("revoking a grant takes effect on the next read", func(t *testing.T) {
		created := f.makeTenant(t, "g4")
		f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/grants", created.ID),
			`{"contract_id":"`+contractA+`"}`)

		rec := f.do(t, http.MethodDelete,
			fmt.Sprintf("/admin/tenants/%d/grants/%s", created.ID, contractA), "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), contractA,
			"the post-revoke grant list must not name the revoked contract")
	})

	t.Run("revoking a grant the tenant does not hold is not an error", func(t *testing.T) {
		created := f.makeTenant(t, "g5")
		rec := f.do(t, http.MethodDelete,
			fmt.Sprintf("/admin/tenants/%d/grants/%s", created.ID, contractB), "")
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestTenantKeyHelpers(t *testing.T) {
	f := newCRUDPFixture(t)

	t.Run("creating a key returns it once and stores only a hash", func(t *testing.T) {
		created := f.makeTenant(t, "k1")
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/keys", created.ID),
			`{"name":"primary"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var minted store.TenantAPIKey
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &minted))
		assert.NotZero(t, minted.ID)
		assert.Equal(t, "primary", minted.Name)
		assert.NotEmpty(t, minted.Secret, "the plaintext appears exactly here")
		assert.Contains(t, minted.Secret, keyScheme+"_", "keys carry the scheme prefix")

		stored := f.tenants.createdKeys[created.ID]
		require.Len(t, stored, 1)
		assert.Empty(t, stored[0].Secret, "the stored record must not carry the plaintext")

		// What the fake was handed is the SHA-256 of the plaintext, never
		// the plaintext itself — that is the hash-only design.
		require.NotEmpty(t, f.tenants.digests[stored[0].ID])
		assert.Len(t, f.tenants.digests[stored[0].ID], 32, "digest is a SHA-256 sum")
		assert.NotEqual(t, []byte(minted.Secret), f.tenants.digests[stored[0].ID])
	})

	t.Run("listing keys never returns a plaintext secret", func(t *testing.T) {
		created := f.makeTenant(t, "k2")
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/keys", created.ID),
			`{"name":"second"}`)
		require.Equal(t, http.StatusCreated, rec.Code)
		var minted store.TenantAPIKey
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &minted))

		rec = f.do(t, http.MethodGet, fmt.Sprintf("/admin/tenants/%d/keys", created.ID), "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), minted.Prefix)
		assert.NotContains(t, rec.Body.String(), minted.Secret,
			"a list response must not leak the plaintext")
	})

	t.Run("deleting a key immediately rejects requests using it", func(t *testing.T) {
		created := f.makeTenant(t, "k3")
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/keys", created.ID),
			`{"name":"shortlived"}`)
		require.Equal(t, http.StatusCreated, rec.Code)
		var minted store.TenantAPIKey
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &minted))

		rec = f.do(t, http.MethodDelete, fmt.Sprintf("/admin/keys/%d", minted.ID), "")
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.True(t, f.tenants.revokedKeys[minted.ID])

		// The revoked prefix no longer authenticates: the same credential
		// that worked moments ago is refused on the very next request.
		req := httptest.NewRequest(http.MethodGet, "/tenant", nil)
		req.Header.Set("Authorization", "Bearer "+minted.Secret)
		revRec := httptest.NewRecorder()
		f.srv.ServeHTTP(revRec, req)
		assert.Equal(t, http.StatusUnauthorized, revRec.Code,
			"a revoked key must be rejected at the door")
	})

	t.Run("revoking an unknown key returns 404", func(t *testing.T) {
		rec := f.do(t, http.MethodDelete, "/admin/keys/987654", "")
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// The scope a tenant resolves to must track the grant helpers: a fresh
// grant widens the next read, a revocation narrows it. This is the bridge
// between the admin helpers and the boundary the read paths enforce.
func TestGrantHelpers_MoveTheResolvedScope(t *testing.T) {
	f := newCRUDPFixture(t)
	created := f.makeTenant(t, "scope")

	f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/grants", created.ID),
		`{"contract_id":"`+contractA+`"}`)
	f.do(t, http.MethodPost, fmt.Sprintf("/admin/tenants/%d/grants", created.ID),
		`{"contract_id":"`+contractB+`"}`)

	scope, err := f.tenants.ScopeForTenant(context.Background(), f.tenants.tenants[created.ID])
	require.NoError(t, err)
	assert.True(t, scope.Allows(contractA))
	assert.True(t, scope.Allows(contractB))

	f.do(t, http.MethodDelete, fmt.Sprintf("/admin/tenants/%d/grants/%s", created.ID, contractA), "")
	scope, err = f.tenants.ScopeForTenant(context.Background(), f.tenants.tenants[created.ID])
	require.NoError(t, err)
	assert.False(t, scope.Allows(contractA), "revocation must reach the resolved scope")
	assert.True(t, scope.Allows(contractB), "an unrelated grant must survive the revocation")
}

// --- helpers ---

func (f *crudpFixture) makeTenant(t *testing.T, name string) store.Tenant {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/admin/tenants", `{"name":"`+name+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Tenant
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	return created
}
