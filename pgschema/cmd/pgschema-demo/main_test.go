// These tests guard the two hand-rolled, un-typechecked surfaces in this
// package: that the html/template scenario list actually renders (a broken
// template only fails at request time, never at `go build`), and that the
// SSE bytes patchSignals/patchElements write match Datastar's documented
// wire protocol byte-for-byte (verified against
// https://data-star.dev/reference/sse_events — there's no compiler check
// tying this string-building to what the datastar.js client expects).
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexRenders(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s := &server{}
	s.handleIndex(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"1. Add nullable column", "6. PG18 bonus", "@post('/scenario/5b/start')", "datastar@v1.0.2"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}

func TestSSEWireFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	f, _ := any(rec).(http.Flusher)
	if err := patchSignals(rec, f, map[string]any{"phase": "starting", "percent": 20}); err != nil {
		t.Fatal(err)
	}
	patchElements(rec, f, "#log-lines", "append", "<div>hi</div>")
	got := rec.Body.String()
	wantSignals := "event: datastar-patch-signals\ndata: signals {\"percent\":20,\"phase\":\"starting\"}\n\n"
	wantElements := "event: datastar-patch-elements\ndata: selector #log-lines\ndata: mode append\ndata: elements <div>hi</div>\n\n"
	if !strings.Contains(got, wantSignals) {
		t.Errorf("signals mismatch:\ngot:  %q\nwant: %q", got, wantSignals)
	}
	if !strings.Contains(got, wantElements) {
		t.Errorf("elements mismatch:\ngot:  %q\nwant: %q", got, wantElements)
	}
}
