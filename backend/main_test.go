package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	ok := func(context.Context) error { return nil }
	bad := func(context.Context) error { return errors.New("down") }
	cases := []struct {
		name   string
		checks map[string]func(context.Context) error
		want   int
	}{
		{"all up", map[string]func(context.Context) error{"postgres": ok, "redis": ok}, 200},
		{"postgres down", map[string]func(context.Context) error{"postgres": bad, "redis": ok}, 503},
		{"redis down", map[string]func(context.Context) error{"postgres": ok, "redis": bad}, 503},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		newMux(c.checks).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}
