/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/purushpsm147/palimpsest/internal/ports"
)

func TestSink_PostsJSONBody(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := New(srv.URL)
	ev := ports.AnomalyEvent{Substrate: "deviation", SeriesID: 7, Value: 3.5}
	if err := sink.EmitAnomaly(context.Background(), ev); err != nil {
		t.Fatalf("EmitAnomaly: %v", err)
	}

	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	var got ports.AnomalyEvent
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SeriesID != 7 || got.Value != 3.5 {
		t.Fatalf("got = %+v, want SeriesID=7 Value=3.5", got)
	}
}

func TestSink_NonSuccessStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := New(srv.URL)
	if err := sink.EmitAnomaly(context.Background(), ports.AnomalyEvent{}); err == nil {
		t.Fatalf("EmitAnomaly: got nil error, want one for a 500 response")
	}
}
