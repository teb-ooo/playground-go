//go:build kratoslive

package testkit_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/teb-ooo/playground-go/testkit"
)

// Run with: KRATOS_ADMIN=http://127.0.0.1:14434 KRATOS_PUBLIC=http://127.0.0.1:14433 IDENTITY_ID=... go test -tags kratoslive -run Live ./testkit
func TestLiveKratos(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	admin, pub, id := os.Getenv("KRATOS_ADMIN"), os.Getenv("KRATOS_PUBLIC"), os.Getenv("IDENTITY_ID")
	tok, terr := testkit.MintKratosSession(context.Background(), admin, id, testkit.WithKratosPublicURL(pub))
	if os.Getenv("EXPECT_TOKEN_ERROR") != "" {
		t.Logf("token error (expected): %v", terr)
		if terr == nil {
			t.Fatal("expected error")
		}
	} else {
		if terr != nil {
			t.Fatal(terr)
		}
		req, _ := http.NewRequest("GET", pub+"/sessions/whoami", nil)
		req.Header.Set("X-Session-Token", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		t.Logf("whoami with token: %d %.200s", resp.StatusCode, b)
		if resp.StatusCode != 200 {
			t.Fatal("token not accepted")
		}
	}
	c, err := testkit.MintKratosSessionCookie(context.Background(), admin, id, testkit.WithKratosPublicURL(pub))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", pub+"/sessions/whoami", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	t.Logf("whoami with cookie: %d %.200s (cookie name=%s domain=%q)", resp.StatusCode, b, c.Name, c.Domain)
	if resp.StatusCode != 200 {
		t.Fatal("cookie not accepted")
	}
}
