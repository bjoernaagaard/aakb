package service

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SokolskyNikita/annas-mcp/anna"
)

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	return nil
}

func searchOptions(in SearchInput) (anna.SearchOptions, error) {
	var fields []anna.SearchField
	for _, f := range []struct{ name, value string }{
		{"title", in.Title}, {"author", in.Author},
		{"publisher", in.Publisher}, {"year", in.Year},
	} {
		if strings.TrimSpace(f.value) != "" {
			fields = append(fields, anna.SearchField{Name: f.name, Value: f.value})
		}
	}
	page := in.Page
	if page == 0 {
		page = 1
	}
	if page < 1 {
		return anna.SearchOptions{}, fmt.Errorf("[INVALID_ARGUMENT] page must be at least 1")
	}
	return anna.SearchOptions{
		Page:         page,
		Sort:         strings.TrimSpace(in.Sort),
		Format:       splitList(in.Format),
		Language:     splitList(in.Language),
		Source:       splitList(in.Source),
		Content:      splitList(in.Content),
		Fields:       fields,
		Descriptions: in.SearchDescriptions,
	}, nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func finishPage[T any](kind string, in SearchInput, results, partial []T,
	total int, lowerBound, morePages bool, conv func(T) *Record) *SearchOutput {
	out := &SearchOutput{Kind: kind, Total: total, TotalIsLowerBound: lowerBound, MorePages: morePages}
	out.Results = convertAll(results, conv)
	out.PartialMatches = convertAll(partial, conv)
	if in.DownloadableOnly {
		out.Results = filterDownloadable(out.Results)
		out.PartialMatches = filterDownloadable(out.PartialMatches)
	}
	out.Results = dedupe(out.Results)
	if in.Rank && in.Sort == "" {
		rank(out.Results)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if len(out.Results) > limit {
		out.Results = out.Results[:limit]
	}
	if morePages {
		page := in.Page
		if page == 0 {
			page = 1
		}
		out.NextPage = page + 1
	}
	return out
}

func convertAll[T any](items []T, conv func(T) *Record) []*Record {
	if len(items) == 0 {
		return nil
	}
	out := make([]*Record, 0, len(items))
	for _, it := range items {
		out = append(out, conv(it))
	}
	return out
}

func filterDownloadable(in []*Record) []*Record {
	out := make([]*Record, 0, len(in))
	for _, r := range in {
		if r.FastDownload {
			out = append(out, r)
		}
	}
	return out
}

// dedupe drops repeat copies of the same file, then repeat editions with
// the same title, author, format and size, keeping the first seen.
func dedupe(in []*Record) []*Record {
	seen := make(map[string]bool, len(in))
	out := make([]*Record, 0, len(in))
	for _, r := range in {
		key := r.Hash
		if key == "" {
			key = strings.Join([]string{
				strings.ToLower(r.Title), strings.ToLower(r.Authors),
				strings.ToLower(r.Format), r.Size,
			}, "|")
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// rank scores availability and reported quality. Popularity counts only
// when stats were requested, so unranked fields keep upstream order.
func rank(in []*Record) {
	for _, r := range in {
		score := 0
		if r.FastDownload {
			score += 100
		}
		if r.PossibleIssues {
			score -= 50
		}
		if r.Stats != nil {
			score += min(r.Stats.Downloads, 40)
			score += min(r.Stats.GreatQuality, 10)
			score -= min(r.Stats.Reports, 20)
		}
		r.Score = score
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Score > in[j].Score })
}

// attachStats fills Stats from one bounded lookup. A failed lookup
// leaves Stats nil, which means unknown, not zero.
func (s *Service) attachStats(ctx context.Context, out *SearchOutput, ranked bool) {
	var hashes []string
	for _, r := range append(out.Results, out.PartialMatches...) {
		if r.Hash != "" {
			hashes = append(hashes, r.Hash)
		}
	}
	if len(hashes) == 0 {
		return
	}
	stats, err := s.client.FileStats(ctx, hashes)
	if err != nil {
		return
	}
	for _, r := range append(out.Results, out.PartialMatches...) {
		if st, ok := stats[r.Hash]; ok {
			st := st
			r.Stats = &st
		}
	}
	if ranked {
		rank(out.Results)
	}
}

func bookRecord(b *anna.Book) *Record {
	return &Record{
		Kind: "book", Hash: b.Hash, Title: b.Title, Authors: b.Authors,
		Publisher: b.Publisher, Edition: b.Edition, Year: b.Year,
		Language: b.Language, Format: b.Format, Size: b.Size, Content: b.Content,
		Sources: b.Sources, DOI: b.DOI, URL: b.URL,
		FastDownload: b.FastDownload, PossibleIssues: b.PossibleIssues, Stats: b.Stats,
	}
}

func paperRecord(p *anna.Paper) *Record {
	return &Record{
		Kind: "article", Hash: p.Hash, Title: p.Title, Authors: p.Authors,
		Publisher: p.Journal, Year: p.Year, Format: p.Format, Size: p.Size,
		Sources: p.Sources, DOI: p.DOI, URL: p.PageURL,
		FastDownload: p.FastDownload, PossibleIssues: p.PossibleIssues, Stats: p.Stats,
	}
}

func metadataRecord(m *anna.MetadataRecord) *Record {
	return &Record{
		Kind: "metadata", ID: m.ID, Title: m.Title, Authors: m.Authors,
		Publisher: m.Publisher, Edition: m.Edition, Year: m.Year,
		Language: m.Language, URL: m.URL,
	}
}
