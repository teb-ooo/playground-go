package embed

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveService runs against a real embeddings service when PLAYGROUND_TEST_EMBED_URL is set (the platform runbook uses
// the container's fixed address, http://172.19.0.15:8080); it skips otherwise.
func TestLiveService(t *testing.T) {
	u := os.Getenv("PLAYGROUND_TEST_EMBED_URL")
	if u == "" {
		t.Skip("PLAYGROUND_TEST_EMBED_URL not set")
	}
	c := New(u, WithTimeout(60*time.Second))
	ctx := context.Background()
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 70) // three batches
	for i := range texts {
		texts[i] = "A note about gardening and tomatoes, number " + string(rune('a'+i%26))
	}
	got, err := c.Embed(ctx, texts, Passage)
	if err != nil || len(got) != 70 || len(got[0]) != 768 {
		t.Fatalf("%d vectors, err %v", len(got), err)
	}
	q, err := c.Embed(ctx, []string{"how do I grow tomatoes"}, Query)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := c.Embed(ctx, []string{"The quarterly tax filing deadline for small businesses"}, Passage)
	if cosine(q[0], got[0]) <= cosine(q[0], other[0]) {
		t.Fatalf("related passage (%f) must be closer than an unrelated one (%f)", cosine(q[0], got[0]), cosine(q[0], other[0]))
	}
}
