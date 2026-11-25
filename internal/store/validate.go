package store

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const maxTEIXMLDepth = 256

// ValidationMode selects content magic-byte and structure verification rules.
type ValidationMode int

const (
	// ValidationGeneral rejects HTML challenge responses without requiring a specific format magic header.
	ValidationGeneral ValidationMode = iota
	// ValidationPDF rejects HTML responses and requires a "%PDF-" prefix.
	ValidationPDF
	// ValidationTEIXML rejects HTML responses and validates streaming TEI XML without DTDs.
	ValidationTEIXML
)

// LooksLikeHTML reports whether a response looks like an HTML document or browser
// challenge based on its Content-Type header or leading bytes.
func LooksLikeHTML(contentType string, prefix []byte) bool {
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err == nil && (mediaType == "text/html" || strings.HasSuffix(mediaType, "+html")) {
			return true
		}
	}
	lower := bytes.ToLower(bytes.TrimSpace(prefix))
	return bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html"))
}

// ValidatePDFPrefix reports whether prefix begins with the PDF magic header.
func ValidatePDFPrefix(prefix []byte) bool {
	return bytes.HasPrefix(prefix, []byte("%PDF-"))
}

// ValidateTEIXML streams r with encoding/xml and verifies that it is a single-root
// TEI (or HTML-wrapped TEI) XML document containing <text> inside <TEI>, with no
// DTD directives and nesting depth at most 256.
func ValidateTEIXML(r io.Reader) error {
	decoder := xml.NewDecoder(r)
	root, hasText, roots := "", false, 0
	depth, teiDepth := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return httpx.NewError(httpx.CodeUpstream, "invalid_tei_xml", "download did not contain valid structured XML")
		}
		switch value := token.(type) {
		case xml.Directive:
			return httpx.NewError(httpx.CodeUpstream, "xml_dtd_forbidden", "structured XML must not contain a document type declaration")
		case xml.StartElement:
			if depth == 0 {
				roots++
				if root == "" {
					root = strings.ToLower(value.Name.Local)
				}
			}
			depth++
			if depth > maxTEIXMLDepth {
				return httpx.NewError(httpx.CodeUpstream, "xml_depth_exceeded", "structured XML nesting limit exceeded")
			}
			name := strings.ToLower(value.Name.Local)
			if name == "tei" && teiDepth == 0 {
				teiDepth = depth
			}
			if name == "text" && teiDepth > 0 {
				hasText = true
			}
		case xml.EndElement:
			if depth == teiDepth {
				teiDepth = 0
			}
			depth--
		}
	}
	if roots != 1 || (root != "tei" && root != "html") || !hasText {
		return httpx.NewError(httpx.CodeUpstream, "missing_tei_text", "download did not contain TEI full text")
	}
	return nil
}

// ContentRangeStart parses a Content-Range header such as "bytes 1000-1999/2000"
// or "bytes 1000-1999/*", returning the starting offset, total size (-1 for "*"),
// and whether the header was valid.
func ContentRangeStart(value string) (start, total int64, ok bool) {
	spec, hasPrefix := strings.CutPrefix(strings.TrimSpace(value), "bytes ")
	if !hasPrefix {
		return 0, -1, false
	}
	rangePart, totalPart, hasSlash := strings.Cut(spec, "/")
	if !hasSlash {
		return 0, -1, false
	}
	first, _, hasDash := strings.Cut(rangePart, "-")
	if !hasDash {
		return 0, -1, false
	}
	parsedStart, err := strconv.ParseInt(first, 10, 64)
	if err != nil || parsedStart < 0 {
		return 0, -1, false
	}
	if totalPart == "*" {
		return parsedStart, -1, true
	}
	parsedTotal, err := strconv.ParseInt(totalPart, 10, 64)
	if err != nil || parsedTotal < 0 {
		return 0, -1, false
	}
	return parsedStart, parsedTotal, true
}
