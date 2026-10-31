// Package omdb talks to the OMDb API (www.omdbapi.com), a database built from
// IMDb's data.
package omdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultBaseURL is the OMDb API endpoint.
const DefaultBaseURL = "https://www.omdbapi.com/"

// ErrNotFound means OMDb has no title for the ID or search.
var ErrNotFound = errors.New("omdb: not found")

// ErrUnauthorized means OMDb refused the API key, or the key ran out of
// requests for the day.
var ErrUnauthorized = errors.New("omdb: API key refused or out of requests")

// Client calls OMDb with one API key.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	APIKey  string
}

// New returns a client for the real OMDb API.
func New(apiKey string) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: DefaultBaseURL,
		APIKey:  apiKey,
	}
}

// Rating is one entry of OMDb's Ratings list.
type Rating struct {
	Source string `json:"Source"`
	Value  string `json:"Value"`
}

// Title is OMDb's full record for one title. Missing values are "N/A".
type Title struct {
	Title      string   `json:"Title"`
	Year       string   `json:"Year"`
	Rated      string   `json:"Rated"`
	Released   string   `json:"Released"`
	Runtime    string   `json:"Runtime"`
	Genre      string   `json:"Genre"`
	Director   string   `json:"Director"`
	Writer     string   `json:"Writer"`
	Actors     string   `json:"Actors"`
	Plot       string   `json:"Plot"`
	Language   string   `json:"Language"`
	Country    string   `json:"Country"`
	Poster     string   `json:"Poster"`
	Ratings    []Rating `json:"Ratings"`
	IMDbRating string   `json:"imdbRating"`
	IMDbID     string   `json:"imdbID"`
	Type       string   `json:"Type"`
	Production string   `json:"Production"`
}

// SearchHit is one entry of an OMDb search.
type SearchHit struct {
	Title  string `json:"Title"`
	Year   string `json:"Year"`
	IMDbID string `json:"imdbID"`
	Type   string `json:"Type"`
	Poster string `json:"Poster"`
}

// envelope carries OMDb's success flag and error, which it sends with HTTP 200.
type envelope struct {
	Response string `json:"Response"`
	Error    string `json:"Error"`
}

// ByID loads one title by its IMDb ID.
func (c *Client) ByID(ctx context.Context, imdbID string) (Title, error) {
	var out struct {
		envelope
		Title
	}
	err := c.get(ctx, url.Values{"i": {imdbID}, "plot": {"full"}}, &out, &out.envelope)
	return out.Title, err
}

// Search finds titles by name, optionally narrowed to a year and a type
// ("movie", "series").
func (c *Client) Search(ctx context.Context, title string, year int, kind string) ([]SearchHit, error) {
	q := url.Values{"s": {title}}
	if year > 0 {
		q.Set("y", fmt.Sprint(year))
	}
	if kind != "" {
		q.Set("type", kind)
	}
	var out struct {
		envelope
		Search []SearchHit `json:"Search"`
	}
	err := c.get(ctx, q, &out, &out.envelope)
	return out.Search, err
}

func (c *Client) get(ctx context.Context, q url.Values, out any, env *envelope) error {
	q.Set("apikey", c.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL carries the API key; never let it reach a log.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("omdb request failed: %w", urlErr.Err)
		}
		return errors.New("omdb request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("omdb answered HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode omdb response: %w", err)
	}
	if env.Response != "True" {
		switch env.Error {
		case "Movie not found!", "Incorrect IMDb ID.", "Series not found!", "Too many results.":
			return ErrNotFound
		case "Invalid API key!", "Request limit reached!", "No API key provided.":
			return ErrUnauthorized
		}
		return fmt.Errorf("omdb error: %s", env.Error)
	}
	return nil
}
