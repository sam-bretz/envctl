package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArtifactClientVerifiesTransferredBytes(t *testing.T) {
	content := []byte("retained checkpoint evidence")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	response := content
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(response) }))
	defer server.Close()
	c := Client{BaseURL: server.URL, HTTP: server.Client()}
	if raw, err := c.Artifact(context.Background(), digest); err != nil || string(raw) != string(content) {
		t.Fatal("valid evidence rejected", err)
	}
	response = []byte("changed evidence")
	if _, err := c.Artifact(context.Background(), digest); err == nil {
		t.Fatal("changed response accepted under original checksum")
	}
	if _, err := c.Artifact(context.Background(), "../outside"); err == nil {
		t.Fatal("invalid artifact identity accepted")
	}
}
