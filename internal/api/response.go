// Package api provides HTTP handlers for the JSON API.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

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

// DecodeJSON reads and decodes a JSON request body into dst.
func DecodeJSON(req *http.Request, dst any) error {
	if req.Body == nil {
		return ErrEmptyBody
	}

	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()

	err := decoder.Decode(dst)
	if err != nil {
		return fmt.Errorf("decoding JSON: %w", err)
	}

	return nil
}
