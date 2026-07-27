package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// HTTPSource is a connector.Source backed by a real HTTP mock (mocksf,
// mockzd) or eventually a real SaaS API. It handles pagination internally
// - callers of Fetch get the complete row set back in one call, matching
// the Source contract every other implementation (fakeSource in exec's
// tests) already honors.
type HTTPSource struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPSource(baseURL string) *HTTPSource {
	return &HTTPSource{BaseURL: strings.TrimSuffix(baseURL, "/")}
}

func (s *HTTPSource) Fetch(ctx context.Context, req FetchRequest) ([]Row, error) {
	var all []Row
	offset := 0
	for {
		page, hasMore, err := s.fetchPage(ctx, req, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if !hasMore {
			break
		}
		offset += len(page)
	}
	return all, nil
}

func (s *HTTPSource) fetchPage(ctx context.Context, req FetchRequest, offset int) ([]Row, bool, error) {
	q := url.Values{}
	if len(req.Columns) > 0 {
		q.Set("fields", strings.Join(req.Columns, ","))
	}
	for col, val := range req.Filters {
		q.Set(col, val)
	}
	q.Set("offset", strconv.Itoa(offset))

	target := fmt.Sprintf("%s/%s?%s", s.BaseURL, localTableName(req.Table), q.Encode())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, fmt.Errorf("connector: build request: %w", err)
	}

	resp, err := s.client().Do(httpReq)
	if err != nil {
		return nil, false, fmt.Errorf("connector: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Rows    []Row `json:"rows"`
			HasMore bool  `json:"has_more"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, false, fmt.Errorf("connector: decode response: %w", err)
		}
		return body.Rows, body.HasMore, nil

	case http.StatusUnprocessableEntity:
		var body struct {
			Field string `json:"field"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return nil, false, &ColumnUnavailableError{Column: body.Field}

	case http.StatusTooManyRequests:
		return nil, false, fmt.Errorf("connector: rate limited, retry after %s", resp.Header.Get("Retry-After"))

	default:
		return nil, false, fmt.Errorf("connector: unexpected status %d", resp.StatusCode)
	}
}

func (s *HTTPSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// localTableName strips the connector prefix ("sf.accounts" -> "accounts")
// - the mock only knows about its own tables, not our qualified names.
func localTableName(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[i+1:]
	}
	return table
}
