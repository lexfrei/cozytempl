// Package api provides HTTP handlers for the JSON API.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// maxJSONBodyBytes is the hard cap on any JSON request body accepted by
// the API layer. Matches the form body limit in the handler package and
// protects against memory-exhaustion DoS from very large payloads.
const maxJSONBodyBytes = 1 << 20 // 1 MiB

// ErrEmptyBody is returned when the request body is nil.
var ErrEmptyBody = errors.New("empty request body")

// JSON writes a JSON response with the given status code.
func JSON(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	err := json.NewEncoder(writer).Encode(data)
	if err != nil {
		slog.Error("encoding JSON response", "error", err)
	}
}

// Error writes a JSON error response.
func Error(writer http.ResponseWriter, status int, msg string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	err := json.NewEncoder(writer).Encode(map[string]string{"error": msg})
	if err != nil {
		slog.Error("encoding JSON error response", "error", err)
	}
}

// DecodeJSON reads and decodes a JSON request body into dst. The body is
// wrapped in http.MaxBytesReader so an attacker can't exhaust memory by
// sending a gigabyte of JSON. Passing writer is required — MaxBytesReader
// needs it so the server can set the right response headers when the
// limit is hit.
func DecodeJSON(writer http.ResponseWriter, req *http.Request, dst any) error {
	if req.Body == nil {
		return ErrEmptyBody
	}

	req.Body = http.MaxBytesReader(writer, req.Body, maxJSONBodyBytes)

	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()

	err := decoder.Decode(dst)
	if err != nil {
		return fmt.Errorf("decoding JSON: %w", err)
	}

	return nil
}
