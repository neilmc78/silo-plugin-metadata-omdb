// Package provider implements Silo's metadata_provider.v1 capability with
// OMDb, for films TMDB does not list (fan films, fan edits, obscure releases).
//
// OMDb's free key allows 1,000 requests a day and Silo asks every metadata
// source on every lookup, so the provider spends requests only where TMDB
// cannot answer: an IMDb ID with no TMDB ID, or a title search the operator
// turned on. Answers are cached, and a daily budget stops the plugin before
// OMDb starts refusing the key.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-metadata-omdb/internal/omdb"
)

// CapabilityID is the metadata_provider.v1 capability id in manifest.json.
// Silo stores the provider's own ID under this key and calls GetMetadata only
// for items that carry it, so films TMDB matched never reach OMDb.
const CapabilityID = "omdb"

// Global config key and fields. They must match manifest.json.
const (
	keyOMDb          = "omdb"
	fieldAPIKey      = "api_key"
	fieldTitleSearch = "title_search"
	fieldDailyLimit  = "daily_limit"
)

const (
	defaultDailyLimit = 900
	cacheTTL          = 24 * time.Hour
	itemTypeMovie     = "movie"
)

var imdbIDPattern = regexp.MustCompile(`^tt\d{7,10}$`)

// errBudgetSpent means the daily request budget is used up.
var errBudgetSpent = errors.New("omdb daily request budget used up; OMDb lookups resume tomorrow (UTC)")

// Config is the plugin's parsed configuration.
type Config struct {
	APIKey      string
	TitleSearch bool
	DailyLimit  int
}

// ParseConfig reads Configure entries. Missing values take their defaults;
// it fails only on values of the wrong type.
func ParseConfig(entries []*pluginv1.ConfigEntry) (Config, error) {
	cfg := Config{DailyLimit: defaultDailyLimit}
	for _, entry := range entries {
		if entry.GetKey() != keyOMDb {
			continue
		}
		values := entry.GetValue().AsMap()
		if raw, ok := values[fieldAPIKey]; ok && raw != nil {
			s, ok := raw.(string)
			if !ok {
				return Config{}, fmt.Errorf("%s.%s must be a string", keyOMDb, fieldAPIKey)
			}
			cfg.APIKey = strings.TrimSpace(s)
		}
		if raw, ok := values[fieldTitleSearch]; ok && raw != nil {
			b, ok := raw.(bool)
			if !ok {
				return Config{}, fmt.Errorf("%s.%s must be a boolean", keyOMDb, fieldTitleSearch)
			}
			cfg.TitleSearch = b
		}
		if raw, ok := values[fieldDailyLimit]; ok && raw != nil {
			n, ok := raw.(float64)
			if !ok {
				return Config{}, fmt.Errorf("%s.%s must be a number", keyOMDb, fieldDailyLimit)
			}
			if n > 0 {
				cfg.DailyLimit = int(n)
			}
		}
	}
	return cfg, nil
}

type cacheEntry struct {
	title    omdb.Title
	notFound bool
	at       time.Time
}

// Provider serves MetadataProvider.
type Provider struct {
	pluginv1.UnimplementedMetadataProviderServer

	logger    *slog.Logger
	newClient func(apiKey string) *omdb.Client
	now       func() time.Time

	mu       sync.Mutex
	cfg      Config
	cache    map[string]cacheEntry
	day      string
	spent    int
	warnedAt string
	// keyChecked records that OMDb accepted the configured key. Settings
	// changes restart the plugin, so it resets with a new key.
	keyChecked bool
	// keyFailedAt holds back re-checking a refused key for an hour, so a bad
	// key does not cost a request on every search.
	keyFailedAt time.Time
}

var _ pluginv1.MetadataProviderServer = (*Provider)(nil)

// New returns a provider that talks to the real OMDb API.
func New(logger *slog.Logger) *Provider {
	return &Provider{
		logger:    logger,
		newClient: omdb.New,
		now:       time.Now,
		cfg:       Config{DailyLimit: defaultDailyLimit},
		cache:     map[string]cacheEntry{},
	}
}

// Configure stores whatever settings arrive. A missing key is reported when a
// lookup needs it, never here.
func (p *Provider) Configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	cfg, err := ParseConfig(entries)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return nil
}

// spend takes one request from today's budget.
func (p *Provider) spend() (Config, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.cfg
	if cfg.APIKey == "" {
		return cfg, errors.New("enter an OMDb API key in the plugin settings")
	}
	today := p.now().UTC().Format(time.DateOnly)
	if p.day != today {
		p.day, p.spent = today, 0
	}
	if p.spent >= cfg.DailyLimit {
		if p.warnedAt != today {
			p.warnedAt = today
			p.logger.Warn("omdb daily request budget used up", "limit", cfg.DailyLimit)
		}
		return cfg, errBudgetSpent
	}
	p.spent++
	return cfg, nil
}

// lookup loads a title by IMDb ID through the cache.
func (p *Provider) lookup(ctx context.Context, imdbID string) (omdb.Title, error) {
	p.mu.Lock()
	entry, ok := p.cache[imdbID]
	p.mu.Unlock()
	if ok && p.now().Sub(entry.at) < cacheTTL {
		if entry.notFound {
			return omdb.Title{}, omdb.ErrNotFound
		}
		return entry.title, nil
	}

	cfg, err := p.spend()
	if err != nil {
		return omdb.Title{}, err
	}
	title, err := p.newClient(cfg.APIKey).ByID(ctx, imdbID)
	if err != nil && !errors.Is(err, omdb.ErrNotFound) {
		return omdb.Title{}, err
	}
	p.mu.Lock()
	p.cache[imdbID] = cacheEntry{title: title, notFound: err != nil, at: p.now()}
	p.mu.Unlock()
	return title, err
}

// keyCheckID is the film looked up to prove the API key works: The Matrix,
// which Silo's connection check also searches for.
const keyCheckID = "tt0133093"

// checkKey proves the API key once per process. Silo's connection check sends
// a title-only search, which otherwise spends nothing, so without this a bad
// key would pass the check.
func (p *Provider) checkKey(ctx context.Context) error {
	p.mu.Lock()
	checked, key, failedAt := p.keyChecked, p.cfg.APIKey, p.keyFailedAt
	p.mu.Unlock()
	if checked {
		return nil
	}
	if key == "" {
		return errors.New("enter an OMDb API key in the plugin settings")
	}
	if !failedAt.IsZero() && p.now().Sub(failedAt) < time.Hour {
		return omdb.ErrUnauthorized
	}
	if _, err := p.lookup(ctx, keyCheckID); err != nil && !errors.Is(err, omdb.ErrNotFound) {
		if errors.Is(err, omdb.ErrUnauthorized) {
			p.mu.Lock()
			p.keyFailedAt = p.now()
			p.mu.Unlock()
		}
		return err
	}
	p.mu.Lock()
	p.keyChecked = true
	p.mu.Unlock()
	return nil
}

// Search answers a match search. It calls OMDb only for an IMDb ID TMDB has
// not resolved (no TMDB ID beside it), or by title when the operator turned
// title search on and no ID was given.
func (p *Provider) Search(ctx context.Context, req *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	resp := &pluginv1.SearchMetadataResponse{}
	if req.GetItemType() != itemTypeMovie {
		return resp, nil
	}
	ids := stringMap(req.GetProviderIds())
	imdbID := strings.TrimSpace(ids["imdb"])
	switch {
	case imdbID != "":
		if ids["tmdb"] != "" || !imdbIDPattern.MatchString(imdbID) {
			return resp, nil
		}
		title, err := p.lookup(ctx, imdbID)
		if errors.Is(err, omdb.ErrNotFound) || errors.Is(err, errBudgetSpent) {
			return resp, nil
		}
		if err != nil {
			return nil, err
		}
		if title.Type != itemTypeMovie {
			return resp, nil
		}
		result, err := searchResult(title.IMDbID, title.Title, title.Year, title.Plot, title.Poster)
		if err != nil {
			return nil, err
		}
		resp.Results = append(resp.Results, result)
	case p.titleSearch() && strings.TrimSpace(req.GetQuery()) != "":
		cfg, err := p.spend()
		if errors.Is(err, errBudgetSpent) {
			return resp, nil
		}
		if err != nil {
			return nil, err
		}
		hits, err := p.newClient(cfg.APIKey).Search(ctx, strings.TrimSpace(req.GetQuery()), int(req.GetYear()), itemTypeMovie)
		if errors.Is(err, omdb.ErrNotFound) {
			return resp, nil
		}
		if err != nil {
			return nil, err
		}
		for _, hit := range hits {
			if !imdbIDPattern.MatchString(hit.IMDbID) {
				continue
			}
			result, err := searchResult(hit.IMDbID, hit.Title, hit.Year, "", hit.Poster)
			if err != nil {
				return nil, err
			}
			resp.Results = append(resp.Results, result)
		}
	default:
		if err := p.checkKey(ctx); err != nil && !errors.Is(err, errBudgetSpent) {
			return nil, err
		}
	}
	return resp, nil
}

func (p *Provider) titleSearch() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.TitleSearch
}

// GetMetadata loads a film Silo matched through this provider. Silo calls it
// only for items carrying this provider's ID, which is the IMDb ID.
func (p *Provider) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	if req.GetItemType() != itemTypeMovie {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	imdbID := req.GetProviderId()
	if imdbID == "" {
		imdbID = stringMap(req.GetProviderIds())[CapabilityID]
	}
	if !imdbIDPattern.MatchString(imdbID) {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	title, err := p.lookup(ctx, imdbID)
	if errors.Is(err, omdb.ErrNotFound) {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	item, err := metadataItem(title)
	if err != nil {
		return nil, err
	}
	return &pluginv1.GetMetadataResponse{Item: item}, nil
}

// GetImages returns the poster, the only artwork OMDb has.
func (p *Provider) GetImages(ctx context.Context, req *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	resp := &pluginv1.GetImagesResponse{}
	if req.GetItemType() != itemTypeMovie || req.SeasonNumber != nil {
		return resp, nil
	}
	imdbID := req.GetProviderId()
	if imdbID == "" {
		imdbID = stringMap(req.GetProviderIds())[CapabilityID]
	}
	if !imdbIDPattern.MatchString(imdbID) {
		return resp, nil
	}
	title, err := p.lookup(ctx, imdbID)
	if errors.Is(err, omdb.ErrNotFound) {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	if poster := value(title.Poster); poster != "" {
		resp.Images = append(resp.Images, &pluginv1.ImageRecord{Kind: "poster", Url: poster})
	}
	return resp, nil
}

func searchResult(imdbID, name, year, plot, poster string) (*pluginv1.ProviderSearchResult, error) {
	ids, err := structpb.NewStruct(map[string]any{"imdb": imdbID})
	if err != nil {
		return nil, err
	}
	return &pluginv1.ProviderSearchResult{
		ProviderId:  imdbID,
		ItemType:    itemTypeMovie,
		Title:       name,
		Year:        int32(firstYear(year)),
		Overview:    value(plot),
		ProviderIds: ids,
		ImageUrl:    value(poster),
	}, nil
}

func metadataItem(t omdb.Title) (*pluginv1.MetadataItem, error) {
	ids, err := structpb.NewStruct(map[string]any{"imdb": t.IMDbID})
	if err != nil {
		return nil, err
	}
	item := &pluginv1.MetadataItem{
		ProviderId:    t.IMDbID,
		ItemType:      itemTypeMovie,
		Title:         t.Title,
		Year:          int32(firstYear(t.Year)),
		Overview:      value(t.Plot),
		Genres:        list(t.Genre),
		ProviderIds:   ids,
		Runtime:       int32(minutes(t.Runtime)),
		Studios:       list(t.Production),
		Countries:     list(t.Country),
		ContentRating: value(t.Rated),
		PosterPath:    value(t.Poster),
		ReleaseDate:   releaseDate(t.Released),
		People:        people(t),
	}
	if rating, err := strconv.ParseFloat(value(t.IMDbRating), 64); err == nil && rating > 0 {
		item.Ratings, err = structpb.NewStruct(map[string]any{"imdb": rating})
		if err != nil {
			return nil, err
		}
	}
	return item, nil
}

func people(t omdb.Title) []*pluginv1.PersonRecord {
	var records []*pluginv1.PersonRecord
	add := func(names, kind string) {
		for _, name := range list(names) {
			// OMDb writer credits carry a role, as in "Jane Doe (screenplay)".
			if i := strings.Index(name, " ("); i > 0 {
				name = name[:i]
			}
			records = append(records, &pluginv1.PersonRecord{Name: name, Kind: kind, SortOrder: int32(len(records))})
		}
	}
	add(t.Actors, "Actor")
	add(t.Director, "Director")
	add(t.Writer, "Writer")
	return records
}

// value turns OMDb's "N/A" placeholder into an empty string.
func value(s string) string {
	s = strings.TrimSpace(s)
	if s == "N/A" {
		return ""
	}
	return s
}

func list(s string) []string {
	var out []string
	for _, part := range strings.Split(value(s), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// firstYear reads "2010" and the series form "2010–2015".
func firstYear(s string) int {
	s = value(s)
	if len(s) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(s[:4])
	return year
}

// minutes reads "140 min".
func minutes(s string) int {
	n, _ := strconv.Atoi(strings.TrimSuffix(value(s), " min"))
	return n
}

// releaseDate turns "19 May 2010" into 2010-05-19.
func releaseDate(s string) string {
	t, err := time.Parse("02 Jan 2006", value(s))
	if err != nil {
		return ""
	}
	return t.Format(time.DateOnly)
}

func stringMap(s *structpb.Struct) map[string]string {
	out := map[string]string{}
	for k, v := range s.AsMap() {
		if text, ok := v.(string); ok {
			out[k] = text
		}
	}
	return out
}
