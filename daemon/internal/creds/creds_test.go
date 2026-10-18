package creds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestRegisterSaveLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/daemon/register" || b["code"] != "good" {
			http.Error(w, `{"error":"invalid or expired registration code"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"machineId": "mch_1", "token": "hm_x"})
	}))
	defer srv.Close()

	if _, err := Register(context.Background(), srv.URL, "bad", "box"); err == nil {
		t.Fatal("bad code must fail")
	}
	c, err := Register(context.Background(), srv.URL+"/", "good", "box")
	if err != nil || c.Token != "hm_x" || c.MachineID != "mch_1" || c.Server != srv.URL {
		t.Fatalf("got %+v %v", c, err)
	}
	home := t.TempDir()
	if err := Save(home, c); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(Path(home))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	got, err := Load(home)
	if err != nil || got != c {
		t.Fatalf("load: %+v %v", got, err)
	}
	_ = os.Chmod(Path(home), 0o644)
	if _, err := Load(home); err == nil {
		t.Fatal("world-readable credentials must be refused")
	}
}
