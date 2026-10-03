package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Google News</title>
    <item>
      <title>Space agency launches new rover mission - SpaceNews</title>
      <link>https://example.com/article1</link>
      <pubDate>Mon, 28 Sep 2026 12:00:00 GMT</pubDate>
      <source url="https://spacenews.example">SpaceNews</source>
    </item>
    <item>
      <title>Breakthrough in fusion energy research - ScienceDaily</title>
      <link>https://example.com/article2</link>
      <pubDate>Mon, 28 Sep 2026 11:30:00 GMT</pubDate>
      <source url="https://sciencedaily.example">ScienceDaily</source>
    </item>
    <item>
      <title>Local community garden wins national award - Independent</title>
      <link>https://example.com/article3</link>
      <pubDate>Mon, 28 Sep 2026 10:00:00 GMT</pubDate>
    </item>
  </channel>
</rss>`

func TestParseRSSItems(t *testing.T) {
	articles, err := ParseRSSItems([]byte(sampleRSS), 2)
	if err != nil {
		t.Fatalf("unexpected error parsing RSS: %v", err)
	}

	if len(articles) != 2 {
		t.Fatalf("expected 2 articles due to limit, got %d", len(articles))
	}

	a1 := articles[0]
	if a1.Title != "Space agency launches new rover mission" {
		t.Errorf("expected clean title without source suffix, got %q", a1.Title)
	}
	if a1.Source != "SpaceNews" {
		t.Errorf("expected source 'SpaceNews', got %q", a1.Source)
	}

	// Test 3 items without limit
	articlesAll, err := ParseRSSItems([]byte(sampleRSS), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(articlesAll) != 3 {
		t.Fatalf("expected 3 articles, got %d", len(articlesAll))
	}

	// Article 3 has no <source> tag, should parse from title
	a3 := articlesAll[2]
	if a3.Title != "Local community garden wins national award" {
		t.Errorf("expected title parsed from prefix, got %q", a3.Title)
	}
	if a3.Source != "Independent" {
		t.Errorf("expected source extracted from title, got %q", a3.Source)
	}
}

func TestNewsServiceWithStub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, sampleRSS)
	}))
	defer server.Close()

	svc := NewNewsService(server.Client())
	svc.BaseURL = server.URL

	ctx := context.Background()
	feed, voiceText, err := svc.FetchHeadlines(ctx, NewsHeadlinesParams{
		Limit: 2,
		Topic: "technology",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(feed.Articles) != 2 {
		t.Errorf("expected 2 articles, got %d", len(feed.Articles))
	}

	if !strings.Contains(voiceText, "Space agency launches new rover mission") {
		t.Errorf("voiceText missing expected title: %s", voiceText)
	}

	// Test MCP handler
	handler := svc.Handler()
	toolRes, _, err := handler(ctx, nil, &NewsHeadlinesParams{Limit: 2})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if toolRes.IsError {
		t.Errorf("unexpected tool error: %+v", toolRes)
	}
	if len(toolRes.Content) == 0 {
		t.Fatalf("handler returned empty content")
	}
}
