package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WebSearchParams defines parameters for the web_search tool.
type WebSearchParams struct {
	Query   string   `json:"query,omitempty" jsonschema:"The web search query"`
	Queries []string `json:"queries,omitempty" jsonschema:"Optional search queries"`
}

// SearchSource represents a cited source.
type SearchSource struct {
	Title  string `json:"title,omitempty"`
	Domain string `json:"domain,omitempty"`
	URL    string `json:"url,omitempty"`
}

// SearchResult holds structured web search answers.
type SearchResult struct {
	Query   string         `json:"query"`
	Answer  string         `json:"answer"`
	Sources []SearchSource `json:"sources,omitempty"`
}

// SearchService handles web search via Gemini REST API with Google Search grounding.
type SearchService struct {
	HTTPClient *http.Client
	Endpoint   string
	APIKey     string
	KeyFile    string
}

// NewSearchService returns a new SearchService.
func NewSearchService(client *http.Client, apiKey, keyFile string) *SearchService {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &SearchService{
		HTTPClient: client,
		Endpoint:   "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
		APIKey:     apiKey,
		KeyFile:    keyFile,
	}
}

// resolveAPIKey retrieves the Gemini API key from explicit value, env, or key file.
// It never prints or logs the key.
func (s *SearchService) resolveAPIKey() (string, error) {
	if key := strings.TrimSpace(s.APIKey); key != "" {
		return key, nil
	}
	if key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); key != "" {
		return key, nil
	}

	keyPath := strings.TrimSpace(s.KeyFile)
	if keyPath == "" {
		keyPath = strings.TrimSpace(os.Getenv("GEMINI_API_KEY_FILE"))
	}
	if keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return "", fmt.Errorf("failed to read Gemini key file: %w", err)
		}
		key := strings.TrimSpace(string(data))
		if key != "" {
			return key, nil
		}
	}

	return "", fmt.Errorf("no Gemini API key configured (set GEMINI_API_KEY or GEMINI_API_KEY_FILE)")
}

type geminiReqContent struct {
	Parts []geminiReqPart `json:"parts"`
}

type geminiReqPart struct {
	Text string `json:"text"`
}

type geminiReqTool struct {
	GoogleSearch map[string]any `json:"google_search"`
}

type geminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type geminiGenerationConfig struct {
	ThinkingConfig *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiRequest struct {
	Contents         []geminiReqContent      `json:"contents"`
	Tools            []geminiReqTool         `json:"tools"`
	GenerationConfig *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		GroundingMetadata struct {
			GroundingChunks []struct {
				Web struct {
					URI   string `json:"uri"`
					Title string `json:"title"`
				} `json:"web"`
			} `json:"groundingChunks"`
		} `json:"groundingMetadata"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Search performs Google-grounded search via Gemini 2.5 Flash.
func (s *SearchService) Search(ctx context.Context, query string) (*SearchResult, string, error) {
	trimmedQuery := strings.TrimSpace(query)
	if trimmedQuery == "" {
		return nil, "", fmt.Errorf("search query cannot be empty")
	}

	apiKey, err := s.resolveAPIKey()
	if err != nil {
		return nil, "", err
	}

	gReq := geminiRequest{
		Contents: []geminiReqContent{
			{
				Parts: []geminiReqPart{
					// Without the date the model answers "latest" questions
					// from its training data (a 2024 Sensex close in 2026).
					{Text: fmt.Sprintf("Today is %s. Search the web and answer briefly, for reading aloud: %s",
						time.Now().Format("Monday, 2 January 2006"), trimmedQuery)},
				},
			},
		},
		Tools: []geminiReqTool{
			{GoogleSearch: map[string]any{}},
		},
		GenerationConfig: &geminiGenerationConfig{
			ThinkingConfig: &geminiThinkingConfig{
				ThinkingBudget: 0,
			},
		},
	}

	bodyBytes, err := json.Marshal(gReq)
	if err != nil {
		return nil, "", fmt.Errorf("failed to serialize search request: %w", err)
	}

	// The key goes in a header, never the URL: a *url.Error carries the full
	// URL, and tool errors are returned to the device and the model.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", fmt.Errorf("failed to construct search request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // without the URL
		}
		return nil, "", fmt.Errorf("search request failed: %v", err)
	}
	defer resp.Body.Close()

	var gResp geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		return nil, "", fmt.Errorf("failed to decode search response: %w", err)
	}

	if gResp.Error != nil {
		return nil, "", fmt.Errorf("Gemini search error (status %d): %s", gResp.Error.Code, gResp.Error.Message)
	}

	if len(gResp.Candidates) == 0 || len(gResp.Candidates[0].Content.Parts) == 0 {
		return nil, "", fmt.Errorf("no search results returned")
	}

	candidate := gResp.Candidates[0]
	answerText := strings.TrimSpace(candidate.Content.Parts[0].Text)

	// Extract up to 3 unique sources
	var sources []SearchSource
	seen := make(map[string]bool)
	for _, chunk := range candidate.GroundingMetadata.GroundingChunks {
		uri := strings.TrimSpace(chunk.Web.URI)
		title := strings.TrimSpace(chunk.Web.Title)
		domain := ""

		if uri != "" {
			if u, err := url.Parse(uri); err == nil {
				domain = u.Hostname()
			}
		}

		key := domain
		if key == "" {
			key = title
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		sources = append(sources, SearchSource{
			Title:  title,
			Domain: domain,
			URL:    uri,
		})
		if len(sources) >= 3 {
			break
		}
	}

	result := &SearchResult{
		Query:   trimmedQuery,
		Answer:  answerText,
		Sources: sources,
	}

	// Format voice-friendly response
	var sb strings.Builder
	sb.WriteString(answerText)

	if len(sources) > 0 {
		sb.WriteString("\n\nSources: ")
		labels := make([]string, 0, len(sources))
		for _, src := range sources {
			if src.Title != "" && src.Domain != "" && !strings.EqualFold(src.Title, src.Domain) {
				labels = append(labels, fmt.Sprintf("%s (%s)", src.Title, src.Domain))
			} else if src.Title != "" {
				labels = append(labels, src.Title)
			} else if src.Domain != "" {
				labels = append(labels, src.Domain)
			}
		}
		sb.WriteString(strings.Join(labels, ", "))
	}

	return result, sb.String(), nil
}

// Handler returns an MCP tool handler for web_search.
func (s *SearchService) Handler() func(ctx context.Context, req *mcp.CallToolRequest, params *WebSearchParams) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, params *WebSearchParams) (*mcp.CallToolResult, any, error) {
		q := ""
		if params != nil {
			q = strings.TrimSpace(params.Query)
			if q == "" && len(params.Queries) > 0 {
				q = strings.TrimSpace(params.Queries[0])
			}
		}
		if q == "" {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "Search query is required"}},
			}, nil, nil
		}

		res, voiceText, err := s.Search(ctx, q)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Search error: %s", err.Error())}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: voiceText}},
		}, res, nil
	}
}
