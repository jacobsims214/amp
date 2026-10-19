package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dexapi "github.com/dexidp/dex/api/v2"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"google.golang.org/grpc"
)

// fakeDex implements the password calls authadmin makes; the embedded
// interface panics on anything else.
type fakeDex struct {
	dexapi.DexClient
	hashes map[string][]byte // email -> bcrypt hash
}

func (f *fakeDex) CreatePassword(_ context.Context, in *dexapi.CreatePasswordReq, _ ...grpc.CallOption) (*dexapi.CreatePasswordResp, error) {
	if _, ok := f.hashes[in.Password.Email]; ok {
		return &dexapi.CreatePasswordResp{AlreadyExists: true}, nil
	}
	f.hashes[in.Password.Email] = in.Password.Hash
	return &dexapi.CreatePasswordResp{}, nil
}

func (f *fakeDex) UpdatePassword(_ context.Context, in *dexapi.UpdatePasswordReq, _ ...grpc.CallOption) (*dexapi.UpdatePasswordResp, error) {
	if _, ok := f.hashes[in.Email]; !ok {
		return &dexapi.UpdatePasswordResp{NotFound: true}, nil
	}
	f.hashes[in.Email] = in.NewHash
	return &dexapi.UpdatePasswordResp{}, nil
}

func (f *fakeDex) DeletePassword(_ context.Context, in *dexapi.DeletePasswordReq, _ ...grpc.CallOption) (*dexapi.DeletePasswordResp, error) {
	if _, ok := f.hashes[in.Email]; !ok {
		return &dexapi.DeletePasswordResp{NotFound: true}, nil
	}
	delete(f.hashes, in.Email)
	return &dexapi.DeletePasswordResp{}, nil
}

// fakeUsers is an in-memory userStore.
type fakeUsers struct{ byEmail map[string]*domain.User }

func (f *fakeUsers) GetUserByEmail(_ context.Context, email string) (*domain.User, error) {
	if u, ok := f.byEmail[email]; ok {
		return u, nil
	}
	return nil, repository.ErrNotFound
}

func (f *fakeUsers) ListUsers(context.Context) ([]domain.User, error) {
	out := []domain.User{}
	for _, u := range f.byEmail {
		out = append(out, *u)
	}
	return out, nil
}

func (f *fakeUsers) DeleteUser(_ context.Context, id int) error {
	for e, u := range f.byEmail {
		if u.ID == id {
			delete(f.byEmail, e)
		}
	}
	return nil
}

type recorded struct {
	p authz.Principal
	e domain.AuditEntry
}

func TestPasswordChangesAreAudited(t *testing.T) {
	admin := &domain.User{ID: 1, Email: "root@amp.test", Roles: []string{domain.RoleAdmin}}
	other := &domain.User{ID: 2, Email: "other@amp.test", Roles: []string{domain.RoleAdmin}}
	bob := &domain.User{ID: 7, Email: "bob@amp.test", Roles: []string{domain.RoleMember}}
	dex := &fakeDex{hashes: map[string][]byte{}}
	users := &fakeUsers{byEmail: map[string]*domain.User{admin.Email: admin, other.Email: other}}
	var got []recorded
	s := &server{dex: dex, repo: users, audit: func(_ context.Context, p authz.Principal, e domain.AuditEntry) {
		got = append(got, recorded{p, e})
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/authadmin/users", auth.RequireRole(domain.RoleAdmin, s.listOrCreateUser))
	mux.HandleFunc("/authadmin/users/", auth.RequireRole(domain.RoleAdmin, s.userSub))

	const pw1, pw2 = "first-Secret-pw-1", "second-Secret-pw-2"
	call := func(method, path string, body any, wantCode int) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{Subject: "s", Email: admin.Email, User: admin, APIToken: true}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != wantCode {
			t.Fatalf("%s %s = %d %s, want %d", method, path, rec.Code, rec.Body, wantCode)
		}
	}

	// Create: bob has never signed in, so there's no AMP user yet.
	call("POST", "/authadmin/users", map[string]string{"email": "Bob@amp.test", "password": pw1}, 200)
	// Reset: bob has signed in since.
	users.byEmail[bob.Email] = bob
	call("PATCH", "/authadmin/users/bob@amp.test", map[string]string{"password": pw2}, 200)
	call("DELETE", "/authadmin/users/bob@amp.test", nil, 200)

	// Failed and refused operations write nothing.
	call("POST", "/authadmin/users", map[string]string{"email": "x@amp.test", "password": "short"}, 400)
	call("PATCH", "/authadmin/users/nobody@amp.test", map[string]string{"password": pw2}, 404)
	call("DELETE", "/authadmin/users/nobody@amp.test", nil, 404)

	want := []struct {
		action, targetID string
	}{
		{"user.password.create", ""},
		{"user.password.reset", "7"},
		{"user.password.delete", "7"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d audit entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		r := got[i]
		if r.e.Action != w.action || r.e.TargetType != "user" || r.e.TargetID != w.targetID {
			t.Errorf("entry %d = %s %s/%s, want %s user/%s", i, r.e.Action, r.e.TargetType, r.e.TargetID, w.action, w.targetID)
		}
		if r.p.UserID != admin.ID || r.p.Email != admin.Email || !r.p.ViaToken {
			t.Errorf("entry %d actor = %+v, want the admin via token", i, r.p)
		}
		if r.e.Details["email"] != bob.Email || !strings.Contains(r.e.Summary, bob.Email) {
			t.Errorf("entry %d doesn't name the target: %+v", i, r.e)
		}
		// No secret: neither password nor any hash Dex ever stored.
		raw, _ := json.Marshal(r.e)
		for _, secret := range []string{pw1, pw2, "$2a$", "$2b$", "hash", "password\":"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("entry %d contains %q: %s", i, secret, raw)
			}
		}
	}
	if got[2].e.Details["amp_user_deleted"] != true {
		t.Errorf("delete entry details = %v", got[2].e.Details)
	}
	if _, ok := users.byEmail[bob.Email]; ok {
		t.Error("bob's AMP user was not deleted")
	}
}
