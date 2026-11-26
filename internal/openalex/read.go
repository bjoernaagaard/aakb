package openalex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	defaultReadCharacters = 8000
	maxReadCharacters     = 16000
	maxReadXMLBytes       = 32 << 20
	maxReadCursorBytes    = 2048
)

// Passage is an extracted paragraph with its available section heading.
// Paragraph numbers identify this extraction and are not PDF page numbers.
type Passage struct {
	Section   string `json:"section,omitempty"`
	Paragraph int    `json:"paragraph"`
	Text      string `json:"text"`
}

// ReadResult identifies the source of text, including abstract-only results.
type ReadResult struct {
	OpenAlexID   string    `json:"openalex_id,omitempty"`
	Title        string    `json:"title"`
	URL          string    `json:"url,omitempty"`
	Source       string    `json:"source"`
	Text         string    `json:"text"`
	Passages     []Passage `json:"passages,omitempty"`
	Digest       string    `json:"sha256,omitempty"`
	Continuation string    `json:"continuation,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
}

type readCursor struct {
	ID        string `json:"id"`
	Digest    string `json:"digest"`
	Paragraph int    `json:"paragraph"`
	Offset    int    `json:"offset"`
}

type textParagraph struct {
	section string
	text    string
}

// Read returns a bounded passage with source attribution. It uses OpenAlex's
// hosted structured text and falls back to a clearly labeled abstract when the
// work has no structured full text. Reading never scrapes landing pages.
// Continuation must be replayed with the same ID and refers to the exact
// downloaded artifact, not a path.
func (c *Client) Read(ctx context.Context, st *store.Store, id, continuation string, limit int) (*ReadResult, error) {
	if strings.TrimSpace(id) == "" {
		return nil, readArgumentError("id is required")
	}
	if limit < 0 || limit > maxReadCharacters {
		return nil, readArgumentError("limit must be between 1 and 16000 characters, or zero for the default")
	}
	var cursor readCursor
	if continuation != "" {
		var err error
		cursor, err = decodeReadCursor(continuation)
		if err != nil {
			return nil, readArgumentError(err.Error())
		}
	}
	work, err := c.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if continuation != "" && cursor.ID != work.ShortID() {
		return nil, readArgumentError("reading continuation refers to a different work")
	}
	var paragraphs []textParagraph
	var digest string
	var source string
	var warnings []string
	if !work.HasContent.GrobidXML || work.ContentURLs.GrobidXML == "" {
		if strings.TrimSpace(work.Abstract()) == "" {
			return nil, unreadableWork("no_structured_text", "no structured full text or abstract is available")
		}
		paragraphs = []textParagraph{{section: "Abstract", text: work.Abstract()}}
		sum := sha256.Sum256([]byte(work.Abstract()))
		digest = hex.EncodeToString(sum[:])
		source = "abstract"
		warnings = []string{"Only the abstract is available through this reading route."}
	} else {
		artifact, downloadErr := c.DownloadArtifact(ctx, st, work,
			DownloadCandidate{URL: work.ContentURLs.GrobidXML, Route: "hosted_tei_xml", Hosted: true}, "", "", nil)
		if downloadErr != nil {
			return nil, downloadErr
		}
		paragraphs, digest, err = readArtifact(ctx, artifact.Path)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &httpx.ProviderError{
				Code: httpx.CodeNotFound, Reason: "invalid_structured_text",
				SafeMessage: "could not read the structured full text", Cause: err,
			}
		}
		if len(paragraphs) == 0 {
			return nil, unreadableWork("empty_structured_text", "structured full text contains no readable body paragraphs")
		}
		source = "openalex_tei"
		warnings = []string{"Machine-extracted text can omit content, equations and figures. Paragraph labels are not PDF page numbers."}
	}
	passages, text, nextContinuation, err := readPage(work.ShortID(), digest, paragraphs, limit, continuation)
	if err != nil {
		return nil, readArgumentError(err.Error())
	}
	pageURL := PublicURL(work.LandingURL())
	page := PublicURL(work.ID)
	if page == "" {
		page = pageURL
	}
	out := &ReadResult{OpenAlexID: work.ShortID(), Title: work.DisplayName, URL: page,
		Source: source, Text: text, Passages: passages, Digest: digest, Warnings: warnings}
	if nextContinuation != "" {
		out.Continuation = nextContinuation
	}
	return out, nil
}

func readArgumentError(message string) error {
	return httpx.NewError(httpx.CodeInvalidArgument, "invalid_argument", message)
}

func unreadableWork(reason, message string) error {
	return httpx.NewError(httpx.CodeNotFound, reason, message)
}

// extractTEI reads only paragraphs within the TEI text. Front matter,
// bibliographies, header metadata and instructions outside TEI are excluded.
// The names are case insensitive to support older HTML-wrapped GROBID TEI.
func extractTEI(ctx context.Context, source io.Reader) ([]textParagraph, error) {
	bounded := &io.LimitedReader{R: source, N: maxReadXMLBytes + 1}
	decoder := xml.NewDecoder(bounded)
	decoder.Entity = map[string]string{"nbsp": "\u00a0"}
	var stack []string
	var paragraphs []textParagraph
	var block strings.Builder
	var section string
	var blockName string
	blockDepth := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if bounded.N == 0 {
			return nil, errors.New("structured full text exceeds the reading size limit")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Join(errors.New("invalid structured full text"), err)
		}
		switch value := token.(type) {
		case xml.Directive:
			return nil, errors.New("structured full text contains an unsupported XML directive")
		case xml.StartElement:
			name := strings.ToLower(value.Name.Local)
			stack = append(stack, name)
			if len(stack) > 256 {
				return nil, errors.New("structured full text exceeds the nesting limit")
			}
			if blockDepth != 0 {
				if name == "lb" || name == "br" {
					block.WriteByte(' ')
				}
				continue
			}
			if teiParagraphContext(stack) && isTextBlock(name) {
				blockDepth, blockName = len(stack), name
				block.Reset()
			}
		case xml.CharData:
			if blockDepth != 0 {
				block.Write(value)
			}
		case xml.EndElement:
			if blockDepth == len(stack) {
				text := strings.Join(strings.Fields(block.String()), " ")
				if blockName == "head" {
					section = text
				} else if text != "" {
					paragraphs = append(paragraphs, textParagraph{section: section, text: text})
				}
				blockDepth = 0
			}
			stack = stack[:len(stack)-1]
		}
	}
	return paragraphs, nil
}

func teiParagraphContext(stack []string) bool {
	inTEI, inText := false, false
	for _, name := range stack {
		switch name {
		case "tei":
			inTEI = true
		case "text":
			inText = inTEI
		case "teiheader", "front", "back":
			return false
		}
	}
	return inTEI && inText
}

func isTextBlock(name string) bool {
	switch name {
	case "p", "head", "item", "ab", "cell":
		return true
	default:
		return false
	}
}

func readArtifact(ctx context.Context, artifactPath string) ([]textParagraph, string, error) {
	file, err := os.Open(artifactPath) //nolint:gosec // G304: artifactPath is a store-managed file under the download directory
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // closing read-only structured text file
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxReadXMLBytes {
		return nil, "", errors.New("structured full text is not a supported regular file")
	}
	hasher := sha256.New()
	paragraphs, err := extractTEI(ctx, io.TeeReader(file, hasher))
	if err != nil {
		return nil, "", err
	}
	return paragraphs, hex.EncodeToString(hasher.Sum(nil)), nil
}

func readPage(id, digest string, paragraphs []textParagraph, limit int, continuation string) ([]Passage, string, string, error) {
	if limit == 0 {
		limit = defaultReadCharacters
	}
	if limit < 1 || limit > maxReadCharacters {
		return nil, "", "", errors.New("limit must be between 1 and 16000 characters")
	}
	cursor := readCursor{ID: id, Digest: digest}
	if continuation != "" {
		var err error
		cursor, err = decodeReadCursor(continuation)
		if err != nil {
			return nil, "", "", err
		}
		if cursor.ID != id || cursor.Digest != digest {
			return nil, "", "", errors.New("reading continuation refers to a different work or changed artifact")
		}
	}
	if cursor.Paragraph < 0 || cursor.Paragraph > len(paragraphs) ||
		cursor.Offset < 0 || (cursor.Paragraph == len(paragraphs) && cursor.Offset != 0) {
		return nil, "", "", errors.New("invalid reading continuation position")
	}
	var passages []Passage
	var text strings.Builder
	remaining := limit
	for cursor.Paragraph < len(paragraphs) {
		paragraph := paragraphs[cursor.Paragraph]
		runes := []rune(paragraph.text)
		if cursor.Offset > len(runes) {
			return nil, "", "", errors.New("invalid reading continuation offset")
		}
		if cursor.Offset == len(runes) {
			cursor.Paragraph++
			cursor.Offset = 0
			continue
		}
		separator := 0
		if len(passages) > 0 {
			separator = 2
		}
		if remaining <= separator {
			break
		}
		available := len(runes) - cursor.Offset
		// Keep an ordinary paragraph intact for the next call. An oversized
		// paragraph is the only case that needs an in-paragraph continuation.
		if len(passages) > 0 && available > remaining-separator {
			break
		}
		take := min(available, remaining-separator)
		piece := string(runes[cursor.Offset : cursor.Offset+take])
		passages = append(passages, Passage{Section: paragraph.section, Paragraph: cursor.Paragraph + 1, Text: piece})
		if separator > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(piece)
		remaining -= take + separator
		cursor.Offset += take
		if cursor.Offset < len(runes) {
			break
		}
		cursor.Paragraph++
		cursor.Offset = 0
	}
	var next string
	if cursor.Paragraph < len(paragraphs) {
		encoded, err := json.Marshal(cursor)
		if err != nil {
			return nil, "", "", err
		}
		next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return passages, text.String(), next, nil
}

func decodeReadCursor(raw string) (readCursor, error) {
	var cursor readCursor
	if len(raw) > maxReadCursorBytes {
		return cursor, errors.New("invalid reading continuation")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(decoded, &cursor) != nil ||
		cursor.ID == "" || cursor.Digest == "" || cursor.Paragraph < 0 || cursor.Offset < 0 {
		return cursor, errors.New("invalid reading continuation")
	}
	return cursor, nil
}

// PublicURL retains useful public addresses without exposing token query strings.
func PublicURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	for key := range u.Query() {
		name := strings.ToLower(key)
		if strings.Contains(name, "key") || strings.Contains(name, "token") || strings.Contains(name, "signature") ||
			strings.Contains(name, "credential") || name == "sig" || name == "auth" || name == "authorization" {
			return ""
		}
	}
	return u.String()
}
