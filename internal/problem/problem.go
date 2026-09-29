// Package problem writes RFC 9457 problem+json responses, the error shape
// Huma produces, for handlers that do not go through Huma.
package problem

import (
	"encoding/json"
	"net/http"
)

// Write sends a problem+json response with title, status and detail.
func Write(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": title, "status": status, "detail": detail})
}
