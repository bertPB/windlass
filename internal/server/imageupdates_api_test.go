package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestImageUpdatesAPI(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	if rec := e.do(t, http.MethodPost, "/api/v1/projects", map[string]string{"name": "app"}, cookie); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	const path = "/api/v1/projects/app/image-updates"
	if rec := e.do(t, http.MethodGet, path, nil, cookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"enabled": "yes"}, map[string]any{"enabled": true, "extra": 1}} {
		if rec := e.do(t, http.MethodPut, path, bad, cookie); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad put = %d: %s", rec.Code, rec.Body.String())
		}
	}
	hashViewer(t, e, context.Background())
	login := e.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{"email": "viewer@example.com", "password": "viewerpassword"})
	viewer := sessionCookie(t, login)
	if rec := e.do(t, http.MethodGet, path, nil, viewer); rec.Code != http.StatusOK {
		t.Fatalf("viewer get = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, path, map[string]bool{"enabled": true}, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer put = %d", rec.Code)
	}
	if _, err := e.sqlDB.Exec("UPDATE users SET role = 'member' WHERE email = 'viewer@example.com'"); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		if rec := e.do(t, http.MethodPut, path, map[string]bool{"enabled": enabled}, viewer); rec.Code != http.StatusOK {
			t.Fatalf("member put = %d: %s", rec.Code, rec.Body.String())
		}
		want := `"enabled":false`
		if enabled {
			want = `"enabled":true`
		}
		if rec := e.do(t, http.MethodGet, path, nil, cookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("get after put = %d: %s", rec.Code, rec.Body.String())
		}
	}
	var audits int
	if err := e.sqlDB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'project.image_updates'").Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audits = %d, error = %v", audits, err)
	}
	if rec := e.do(t, http.MethodGet, "/api/v1/projects/missing/image-updates", nil, cookie); rec.Code != http.StatusNotFound {
		t.Fatalf("missing get = %d", rec.Code)
	}
	// Check now: refused while off, queued once on, member only.
	const check = "/api/v1/projects/app/image-updates/check"
	if rec := e.do(t, http.MethodPost, check, nil, cookie); rec.Code != http.StatusConflict {
		t.Fatalf("check while off = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodPut, path, map[string]bool{"enabled": true}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPost, check, nil, cookie); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"result":"queued"`) {
		t.Fatalf("check = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := e.sqlDB.Exec("UPDATE users SET role = 'viewer' WHERE email = 'viewer@example.com'"); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, http.MethodPost, check, nil, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer check = %d", rec.Code)
	}
}
