package rss

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

const feedXML = `<?xml version="1.0"?>
<rss version="2.0"><channel><title>t</title><link>http://example.com</link><description>d</description>
<item><title>post</title><link>http://example.com/1</link></item></channel></rss>`

func TestFetchFeedRetriesTransientFailures(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			// Abrupt close reads as a transient network error to the client.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feed, err := fetchFeedWithRetry(gofeed.NewParser(), srv.URL, []time.Duration{time.Millisecond, time.Millisecond})
	if err != nil {
		t.Fatalf("fetchFeedWithRetry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(feed.Items) != 1 || feed.Items[0].Title != "post" {
		t.Fatalf("unexpected feed: %+v", feed.Items)
	}
}

func TestFetchFeedSkipsPermanentError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchFeedWithRetry(gofeed.NewParser(), srv.URL, []time.Duration{time.Millisecond, time.Millisecond})
	var httpErr gofeed.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want gofeed.HTTPError", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 — a server verdict must not be retried", calls)
	}
}

func TestFetchFailsWhenEveryFeedFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := &RSSSource{Feeds: []FeedConfig{
		{Name: "a", URL: srv.URL + "/a"},
		{Name: "b", URL: srv.URL + "/b"},
	}}
	if _, err := src.Fetch(); err == nil {
		t.Fatal("Fetch() = nil error, want every-feed-failed")
	}
}

func TestFetchSurvivesAPartialFailure(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer ok.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	src := &RSSSource{Feeds: []FeedConfig{
		{Name: "ok", URL: ok.URL},
		{Name: "dead", URL: dead.URL},
	}}
	items, err := src.Fetch()
	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil (one feed worked)", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
}

func TestTransientNetworkError(t *testing.T) {
	if transientNetworkError(gofeed.HTTPError{StatusCode: 404, Status: "Not Found"}) {
		t.Error("HTTPError classified transient")
	}
	if !transientNetworkError(&url.Error{Err: errors.New("dial tcp: network is unreachable")}) {
		t.Error("url.Error not classified transient")
	}
	if transientNetworkError(gofeed.ErrFeedTypeNotDetected) {
		t.Error("content error classified transient")
	}
}
