package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-metadata-omdb/internal/omdb"
)

const fanFilm = "tt18183916"

// fakeOMDb knows one fan film and The Matrix, and counts requests.
type fakeOMDb struct {
	calls atomic.Int32
	key   string
}

func (f *fakeOMDb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	if q.Get("apikey") != f.key {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"Response": "False", "Error": "Invalid API key!"})
		return
	}
	switch q.Get("i") {
	case fanFilm:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Response": "True", "Title": "Star War the Third Gathers: The Backstroke of the West",
			"Year": "2010", "Released": "19 May 2010", "Runtime": "140 min", "Genre": "Action, Comedy, Sci-Fi",
			"Director": "Timothy B. Morgan", "Writer": "Jeremy Winterson (adaptation)", "Actors": "Sum-Yeung Wong, Arielle Ceplina",
			"Plot": "Mistranslated fan dub.", "Poster": "https://example.invalid/poster.jpg", "imdbRating": "8.8",
			"imdbID": fanFilm, "Type": "movie", "Rated": "N/A",
		})
	case keyCheckID:
		_ = json.NewEncoder(w).Encode(map[string]any{"Response": "True", "Title": "The Matrix", "imdbID": keyCheckID, "Type": "movie"})
	default:
		_ = json.NewEncoder(w).Encode(map[string]string{"Response": "False", "Error": "Incorrect IMDb ID."})
	}
}

func newTestProvider(t *testing.T, key string, settings map[string]any) (*Provider, *fakeOMDb) {
	t.Helper()
	fake := &fakeOMDb{key: "good-key"}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.newClient = func(apiKey string) *omdb.Client {
		c := omdb.New(apiKey)
		c.BaseURL = srv.URL + "/"
		return c
	}
	values := map[string]any{fieldAPIKey: key}
	for k, v := range settings {
		values[k] = v
	}
	cfg, err := structpb.NewStruct(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: keyOMDb, Value: cfg}}); err != nil {
		t.Fatal(err)
	}
	return p, fake
}

func search(t *testing.T, p *Provider, query string, ids map[string]any) (*pluginv1.SearchMetadataResponse, error) {
	t.Helper()
	s, err := structpb.NewStruct(ids)
	if err != nil {
		t.Fatal(err)
	}
	return p.Search(context.Background(), &pluginv1.SearchMetadataRequest{Query: query, ItemType: "movie", ProviderIds: s})
}

// Silo asks every source on every lookup; OMDb's free quota survives only if
// the plugin stays quiet where TMDB answers.
func TestSearchSpendsRequestsOnlyWhereTMDBCannotAnswer(t *testing.T) {
	cases := []struct {
		name        string
		query       string
		ids         map[string]any
		settings    map[string]any
		wantResults int
		wantCalls   int32
	}{
		{"imdb id without tmdb id", "", map[string]any{"imdb": fanFilm}, nil, 1, 1},
		{"imdb id that tmdb already resolved", "", map[string]any{"imdb": fanFilm, "tmdb": "1895"}, nil, 0, 0},
		{"unknown imdb id", "", map[string]any{"imdb": "tt0000001"}, nil, 0, 1},
		{"title only checks the key once", "Some Film", map[string]any{}, nil, 0, 1},
		{"daily limit spent", "", map[string]any{"imdb": fanFilm}, map[string]any{fieldDailyLimit: float64(1)}, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newTestProvider(t, "good-key", tc.settings)
			if tc.name == "daily limit spent" {
				// Spend the one allowed request on another title first.
				if _, err := search(t, p, "", map[string]any{"imdb": keyCheckID}); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := search(t, p, tc.query, tc.ids)
			if err != nil {
				t.Fatal(err)
			}
			// A repeat answers from the cache or the guard, never OMDb.
			if _, err := search(t, p, tc.query, tc.ids); err != nil {
				t.Fatal(err)
			}
			if got := len(resp.GetResults()); got != tc.wantResults {
				t.Fatalf("results = %d, want %d", got, tc.wantResults)
			}
			if got := fake.calls.Load(); got != tc.wantCalls {
				t.Fatalf("OMDb calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestSearchReportsARefusedKey(t *testing.T) {
	p, fake := newTestProvider(t, "bad-key", nil)
	for range 3 {
		if _, err := search(t, p, "The Matrix", map[string]any{}); err == nil {
			t.Fatal("a refused key must fail the connection check's search")
		}
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("OMDb calls = %d, want 1: a refused key is not retried on every search", got)
	}
}

func TestGetMetadataMapsTheFilm(t *testing.T) {
	p, _ := newTestProvider(t, "good-key", nil)
	p.now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	resp, err := p.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{ProviderId: fanFilm, ItemType: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	item := resp.GetItem()
	if item.GetYear() != 2010 || item.GetRuntime() != 140 || item.GetReleaseDate() != "2010-05-19" {
		t.Fatalf("year/runtime/release = %d/%d/%q", item.GetYear(), item.GetRuntime(), item.GetReleaseDate())
	}
	if item.GetProviderIds().AsMap()["imdb"] != fanFilm || item.GetContentRating() != "" {
		t.Fatalf("ids %v, content rating %q", item.GetProviderIds().AsMap(), item.GetContentRating())
	}
	if len(item.GetGenres()) != 3 || item.GetRatings().AsMap()["imdb"] != 8.8 {
		t.Fatalf("genres %v, ratings %v", item.GetGenres(), item.GetRatings().AsMap())
	}
	people := item.GetPeople()
	if len(people) != 4 || people[3].GetName() != "Jeremy Winterson" || people[3].GetKind() != "Writer" {
		t.Fatalf("people = %v", people)
	}
}
