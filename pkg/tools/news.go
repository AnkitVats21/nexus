package tools

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewsHeadlinesParams defines the parameters for the news_headlines tool.
type NewsHeadlinesParams struct {
	Topic   string `json:"topic,omitempty" jsonschema:"Topic: world, nation, business, technology, entertainment, sports, science, health"`
	Query   string `json:"query,omitempty" jsonschema:"Search query for specific news stories"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum number of headlines (1-10, default 5)"`
	Lang    string `json:"lang,omitempty" jsonschema:"Language code (e.g. 'en')"`
	Country string `json:"country,omitempty" jsonschema:"Country code (e.g. 'IN')"`
}

// NewsArticle represents a single parsed news story.
type NewsArticle struct {
	Title     string `json:"title"`
	Source    string `json:"source"`
	Published string `json:"published"`
}

// NewsFeed holds structured articles.
type NewsFeed struct {
	Query    string        `json:"query,omitempty"`
	Topic    string        `json:"topic,omitempty"`
	Articles []NewsArticle `json:"articles"`
}

// NewsService handles fetching and parsing Google News RSS feeds.
type NewsService struct {
	HTTPClient *http.Client
	BaseURL    string
}

// NewNewsService returns a new NewsService.
func NewNewsService(client *http.Client) *NewsService {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &NewsService{
		HTTPClient: client,
		BaseURL:    "https://news.google.com/rss",
	}
}

type rssItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	PubDate string `xml:"pubDate"`
	Source  struct {
		Name string `xml:",chardata"`
		URL  string `xml:"url,attr"`
	} `xml:"source"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

var topicMap = map[string]string{
	"world":         "WORLD",
	"nation":        "NATION",
	"business":      "BUSINESS",
	"technology":    "TECHNOLOGY",
	"entertainment": "ENTERTAINMENT",
	"sports":        "SPORTS",
	"science":       "SCIENCE",
	"health":        "HEALTH",
}

// ParseRSSItems parses raw RSS XML bytes into a slice of NewsArticle.
func ParseRSSItems(data []byte, limit int) ([]NewsArticle, error) {
	if limit < 1 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}

	var root rssRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("failed to parse RSS XML: %w", err)
	}

	var articles []NewsArticle
	for _, item := range root.Channel.Items {
		if len(articles) >= limit {
			break
		}

		rawTitle := strings.TrimSpace(item.Title)
		sourceName := strings.TrimSpace(item.Source.Name)

		// Google News titles are usually "Headline - Source Name"
		// If Source tag was empty, extract it from the title
		title := rawTitle
		if sourceName == "" {
			if idx := strings.LastIndex(rawTitle, " - "); idx != -1 {
				title = strings.TrimSpace(rawTitle[:idx])
				sourceName = strings.TrimSpace(rawTitle[idx+3:])
			}
		} else {
			// Strip trailing " - Source Name" from title if present
			suffix := " - " + sourceName
			if strings.HasSuffix(rawTitle, suffix) {
				title = strings.TrimSpace(strings.TrimSuffix(rawTitle, suffix))
			}
		}

		// Format published time (e.g. "Mon, 28 Sep 2026 09:42:19 GMT" -> relative or short date)
		pubStr := strings.TrimSpace(item.PubDate)
		if t, err := time.Parse(time.RFC1123, pubStr); err == nil {
			pubStr = formatTimeAgo(t)
		} else if t, err := time.Parse(time.RFC1123Z, pubStr); err == nil {
			pubStr = formatTimeAgo(t)
		}

		articles = append(articles, NewsArticle{
			Title:     title,
			Source:    sourceName,
			Published: pubStr,
		})
	}

	return articles, nil
}

func formatTimeAgo(t time.Time) string {
	diff := time.Since(t)
	if diff < 0 {
		return t.Format("Jan 2, 15:04 MST")
	}
	if diff < time.Minute {
		return "just now"
	}
	if diff < time.Hour {
		mins := int(diff.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	}
	if diff < 24*time.Hour {
		hrs := int(diff.Hours())
		if hrs == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hrs)
	}
	days := int(diff.Hours() / 24)
	if days == 1 {
		return "yesterday"
	}
	if days < 7 {
		return fmt.Sprintf("%d days ago", days)
	}
	return t.Format("Jan 2")
}

// FetchHeadlines fetches headlines matching query/topic/country/lang.
func (s *NewsService) FetchHeadlines(ctx context.Context, params NewsHeadlinesParams) (*NewsFeed, string, error) {
	limit := params.Limit
	if limit < 1 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}

	lang := strings.TrimSpace(params.Lang)
	if lang == "" {
		lang = "en"
	}
	country := strings.TrimSpace(params.Country)
	if country == "" {
		country = "IN"
	}

	query := strings.TrimSpace(params.Query)
	topic := strings.ToLower(strings.TrimSpace(params.Topic))

	var reqURL string
	var headerLabel string

	commonQuery := fmt.Sprintf("hl=%s-%s&gl=%s&ceid=%s:%s", lang, country, country, country, lang)

	if query != "" {
		reqURL = fmt.Sprintf("%s/search?q=%s&%s", s.BaseURL, url.QueryEscape(query), commonQuery)
		headerLabel = fmt.Sprintf("News for %q", query)
	} else if topic != "" {
		if mapped, ok := topicMap[topic]; ok {
			reqURL = fmt.Sprintf("%s/headlines/section/topic/%s?%s", s.BaseURL, mapped, commonQuery)
		} else {
			// Fallback to search query for unknown topic
			reqURL = fmt.Sprintf("%s/search?q=%s&%s", s.BaseURL, url.QueryEscape(topic), commonQuery)
		}
		headerLabel = fmt.Sprintf("Top %s news", strings.Title(topic))
	} else {
		reqURL = fmt.Sprintf("%s?%s", s.BaseURL, commonQuery)
		headerLabel = "Top news headlines"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create news request: %w", err)
	}
	req.Header.Set("User-Agent", "Nexus-MCP/1.0 (Voice Assistant Data Hub)")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch news: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("news service returned status %d", resp.StatusCode)
	}

	var root rssRoot
	if err := xml.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, "", fmt.Errorf("failed to decode news RSS: %w", err)
	}

	var articles []NewsArticle
	for _, item := range root.Channel.Items {
		if len(articles) >= limit {
			break
		}

		rawTitle := strings.TrimSpace(item.Title)
		sourceName := strings.TrimSpace(item.Source.Name)
		title := rawTitle

		if sourceName == "" {
			if idx := strings.LastIndex(rawTitle, " - "); idx != -1 {
				title = strings.TrimSpace(rawTitle[:idx])
				sourceName = strings.TrimSpace(rawTitle[idx+3:])
			}
		} else {
			suffix := " - " + sourceName
			if strings.HasSuffix(rawTitle, suffix) {
				title = strings.TrimSpace(strings.TrimSuffix(rawTitle, suffix))
			}
		}

		pubStr := strings.TrimSpace(item.PubDate)
		if t, err := time.Parse(time.RFC1123, pubStr); err == nil {
			pubStr = formatTimeAgo(t)
		} else if t, err := time.Parse(time.RFC1123Z, pubStr); err == nil {
			pubStr = formatTimeAgo(t)
		}

		articles = append(articles, NewsArticle{
			Title:     title,
			Source:    sourceName,
			Published: pubStr,
		})
	}

	feed := &NewsFeed{
		Query:    query,
		Topic:    topic,
		Articles: articles,
	}

	if len(articles) == 0 {
		return feed, fmt.Sprintf("%s: No stories found.", headerLabel), nil
	}

	var sb strings.Builder
	sb.WriteString(headerLabel)
	sb.WriteString(":\n")
	for i, a := range articles {
		meta := ""
		if a.Source != "" && a.Published != "" {
			meta = fmt.Sprintf(" (%s, %s)", a.Source, a.Published)
		} else if a.Source != "" {
			meta = fmt.Sprintf(" (%s)", a.Source)
		} else if a.Published != "" {
			meta = fmt.Sprintf(" (%s)", a.Published)
		}
		sb.WriteString(fmt.Sprintf("%d. %s%s\n", i+1, a.Title, meta))
	}

	return feed, strings.TrimSpace(sb.String()), nil
}

// Handler returns an MCP tool handler for news_headlines.
func (s *NewsService) Handler() func(ctx context.Context, req *mcp.CallToolRequest, params *NewsHeadlinesParams) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, params *NewsHeadlinesParams) (*mcp.CallToolResult, any, error) {
		var p NewsHeadlinesParams
		if params != nil {
			p = *params
		}

		feed, voiceText, err := s.FetchHeadlines(ctx, p)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("News error: %s", err.Error())}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: voiceText}},
		}, feed, nil
	}
}
