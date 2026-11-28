package httpapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb"
	"github.com/bjoernaagaard/aakb/internal/adapter"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	maxTimeoutSeconds = 1800
	maxBodyBytes      = 16 << 10
)

func (a *API) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.ContentLength != 0 {
		badRequest(w, "Capabilities does not accept query parameters or a body.")
		return
	}
	caps := a.client.Capabilities()
	writeJSON(w, http.StatusOK, "application/json", capabilitiesResponse{
		Version: a.version, OpenAlex: caps.OpenAlex, DOIMirror: caps.DOIMirror, AuthRequired: len(a.guard.bearer) != 0,
	})
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	query, err := parseQuery(r, "q", "kind", "title", "author", "publisher", "year", "work_type",
		"author_id", "institution_id", "repository_id", "relation", "work_id",
		"format", "language", "source", "content", "sort", "page", "limit",
		"stats", "rank", "downloadable_only", "search_descriptions", "timeout_seconds")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	req, err := parseSearch(query)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	seconds, err := queryInt(query, "timeout_seconds", 0, 0, maxTimeoutSeconds)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := requestContext(r.Context(), seconds)
	defer cancel()
	out, err := a.client.Search(ctx, req)
	a.respond(w, r, out, err)
}

func parseSearch(query url.Values) (aakb.SearchRequest, error) {
	req := aakb.SearchRequest{
		Query: query.Get("q"), Kind: aakb.Kind(query.Get("kind")),
		Title: query.Get("title"), Author: query.Get("author"), Publisher: query.Get("publisher"),
		Year: query.Get("year"), WorkType: query.Get("work_type"),
		Formats: adapter.SplitList(query.Get("format")), Languages: adapter.SplitList(query.Get("language")),
		Sources: adapter.SplitList(query.Get("source")), Content: adapter.SplitList(query.Get("content")),
		Sort: aakb.Sort(query.Get("sort")),
		OpenAlex: aakb.OpenAlexFilter{
			AuthorID: query.Get("author_id"), InstitutionID: query.Get("institution_id"),
			RepositoryID: query.Get("repository_id"), Relation: query.Get("relation"), WorkID: query.Get("work_id"),
		},
	}
	switch req.Kind {
	case aakb.KindAuto, aakb.KindBook, aakb.KindArticle, aakb.KindMetadata, aakb.KindOpenAlex:
	default:
		return req, errors.New("kind must be empty, book, article, metadata or openalex")
	}
	switch req.Sort {
	case aakb.SortRelevance, aakb.SortNewest, aakb.SortOldest, aakb.SortLargest, aakb.SortSmallest,
		aakb.SortNewestAdded, aakb.SortOldestAdded, aakb.SortRandom:
	default:
		return req, errors.New("sort must be empty, newest, oldest, largest, smallest, newest_added, oldest_added or random")
	}

	var err error
	req.Page, err = queryInt(query, "page", 1, 1, math.MaxInt)
	if err != nil {
		return req, err
	}
	req.Limit, err = queryInt(query, "limit", 10, 1, 50)
	if err != nil {
		return req, err
	}
	for _, field := range []struct {
		name string
		dest *bool
	}{
		{"stats", &req.IncludeStats},
		{"rank", &req.Rank},
		{"downloadable_only", &req.DownloadableOnly},
		{"search_descriptions", &req.SearchDescriptions},
	} {
		switch query.Get(field.name) {
		case "true", "1":
			*field.dest = true
		case "false", "0":
		default:
			if query.Has(field.name) {
				return req, fmt.Errorf("%s must be true, false, 1 or 0", field.name)
			}
			*field.dest = field.name == "rank"
		}
	}
	return req, nil
}

func (a *API) lookup(w http.ResponseWriter, r *http.Request) {
	query, err := parseQuery(r, "id", "timeout_seconds")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	id := strings.TrimSpace(query.Get("id"))
	if id == "" {
		badRequest(w, "id is required.")
		return
	}
	seconds, err := queryInt(query, "timeout_seconds", 0, 0, maxTimeoutSeconds)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := requestContext(r.Context(), seconds)
	defer cancel()
	out, err := a.client.Details(ctx, id)
	a.respond(w, r, out, err)
}

func (a *API) account(w http.ResponseWriter, r *http.Request) {
	query, err := parseQuery(r, "timeout_seconds")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	seconds, err := queryInt(query, "timeout_seconds", 0, 0, maxTimeoutSeconds)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := requestContext(r.Context(), seconds)
	defer cancel()
	out, err := a.client.AccountStatus(ctx)
	a.respond(w, r, out, err)
}

func (a *API) readArticle(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeBody[readInput](w, r)
	if !ok || !validTimeout(w, input.TimeoutSeconds) {
		return
	}
	if strings.TrimSpace(input.ID) == "" {
		badRequest(w, "id is required.")
		return
	}
	if input.Limit < 0 || input.Limit > 16000 {
		badRequest(w, "limit must be between 0 and 16000 characters.")
		return
	}
	ctx, cancel := requestContext(r.Context(), input.TimeoutSeconds)
	defer cancel()
	out, err := a.client.ReadArticle(ctx, aakb.ReadRequest{ID: input.ID, Continuation: input.Continuation, Limit: input.Limit})
	a.respond(w, r, out, err)
}

func (a *API) download(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeBody[downloadInput](w, r)
	if !ok || !validTimeout(w, input.TimeoutSeconds) {
		return
	}
	ctx, cancel := requestContext(r.Context(), input.TimeoutSeconds)
	defer cancel()
	out, err := a.client.Download(ctx, aakb.DownloadRequest{
		Hash: input.Hash, DOI: input.DOI, OpenAlexID: input.OpenAlexID, Title: input.Title, Format: input.Format,
		Artifact: input.Artifact, LocationID: input.LocationID, Version: input.Version,
	})
	if err != nil {
		a.writeClientError(w, r, err)
		return
	}
	if !store.ValidFilename(out.Name) {
		writeProblem(w, http.StatusInternalServerError, string(aakb.CodeInternal), "The download has an unsupported filename.")
		return
	}
	fileURL := downloadsPath + url.PathEscape(out.Name)
	w.Header().Set("Location", fileURL)
	status := http.StatusCreated
	if out.Reused {
		status = http.StatusOK
	}
	writeJSON(w, status, "application/json", downloadResponse{
		Name: out.Name, FileURL: fileURL, Bytes: out.Bytes, Verification: out.Verification,
		MD5: out.MD5, SHA256: out.SHA256, Reused: out.Reused, DownloadsLeft: out.DownloadsLeft,
	})
}

func (a *API) respond(w http.ResponseWriter, r *http.Request, value any, err error) {
	if err != nil {
		a.writeClientError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, "application/json", value)
}

func parseQuery(r *http.Request, allowed ...string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("the query string is malformed")
	}
	for name, values := range query {
		if !slices.Contains(allowed, name) {
			return nil, errors.New("the query contains an unknown parameter")
		}
		if len(values) != 1 {
			return nil, errors.New("each query parameter may occur only once")
		}
	}
	return query, nil
}

func queryInt(query url.Values, name string, fallback, low, high int) (int, error) {
	if !query.Has(name) {
		return fallback, nil
	}
	value, err := strconv.Atoi(query.Get(name))
	if name == "page" && (err != nil || value < low) {
		return 0, errors.New("page must be a positive integer")
	}
	if err != nil || value < low || value > high {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, low, high)
	}
	return value, nil
}

func decodeBody[T any](w http.ResponseWriter, r *http.Request) (*T, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Use Content-Type application/json.")
		return nil, false
	}
	if r.URL.RawQuery != "" {
		badRequest(w, "Use the JSON body for this resource's parameters.")
		return nil, false
	}
	var input *T
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.UnmarshalRead(body, &input, json.RejectUnknownMembers(true)); err != nil || input == nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			badRequest(w, "The JSON body must be at most 16 KiB.")
		} else {
			badRequest(w, "Send a single JSON object with the documented fields and value types.")
		}
		return nil, false
	}
	return input, true
}

func badRequest(w http.ResponseWriter, detail string) {
	writeProblem(w, http.StatusBadRequest, string(aakb.CodeInvalidArgument), detail)
}

func validTimeout(w http.ResponseWriter, seconds int) bool {
	if seconds < 0 || seconds > maxTimeoutSeconds {
		badRequest(w, "timeout_seconds must be between 0 and 1800.")
		return false
	}
	return true
}

func requestContext(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds == 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}
