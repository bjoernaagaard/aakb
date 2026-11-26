package archive

import (
	"bytes"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// This file holds the small golang.org/x/net/html helpers every zero-goquery
// HTML parser in this package builds on. Where the archive's own scripts use a js-
// class, the parsers use it too: those change when the archive's scripts do, while
// styling classes change with any redesign. Icons and link targets are the
// next best thing.

// parseHTML parses one HTML document for the search, account, SciDB, and
// record parsers below.
func parseHTML(data []byte) (*html.Node, error) {
	return html.Parse(bytes.NewReader(data))
}

// nodeAttr returns the value of the named attribute, or "" when absent.
func nodeAttr(node *html.Node, key string) string {
	if node == nil || node.Type != html.ElementNode {
		return ""
	}
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

// nodeHasClass reports whether an element carries a single CSS class token.
func nodeHasClass(node *html.Node, class string) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	for _, attr := range node.Attr {
		if attr.Key == "class" && slices.Contains(strings.Fields(attr.Val), class) {
			return true
		}
	}
	return false
}

// nodeText joins all descendant text of a node, as goquery's Text does.
func nodeText(node *html.Node) string {
	if node == nil {
		return ""
	}
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			out.WriteString(current.Data)
		}
		for child := range current.ChildNodes() {
			walk(child)
		}
	}
	walk(node)
	return out.String()
}

// nodeOwnText joins only a node's direct text children, leaving out nested
// elements such as the Save button and statistics scripts the archive appends to
// the metadata line.
func nodeOwnText(node *html.Node) string {
	if node == nil {
		return ""
	}
	var out strings.Builder
	for child := range node.ChildNodes() {
		if child.Type == html.TextNode {
			out.WriteString(child.Data)
		}
	}
	return out.String()
}

// findFirstNode returns the first node in document order matching keep, or nil.
func findFirstNode(root *html.Node, keep func(*html.Node) bool) *html.Node {
	if root == nil {
		return nil
	}
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(current *html.Node) bool {
		if keep(current) {
			found = current
			return false
		}
		for child := range current.ChildNodes() {
			if !walk(child) {
				return false
			}
		}
		return true
	}
	walk(root)
	return found
}

// findNodes returns every node in document order matching keep.
func findNodes(root *html.Node, keep func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	if root == nil {
		return out
	}
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if keep(current) {
			out = append(out, current)
		}
		for child := range current.ChildNodes() {
			walk(child)
		}
	}
	walk(root)
	return out
}

// isElement reports whether node is an element with the given tag name.
func isElement(node *html.Node, tag string) bool {
	return node != nil && node.Type == html.ElementNode && node.Data == tag
}

// withClass matches elements carrying one CSS class token.
func withClass(class string) func(*html.Node) bool {
	return func(node *html.Node) bool {
		return node.Type == html.ElementNode && nodeHasClass(node, class)
	}
}

// elementChildren returns the child elements of a node in order.
func elementChildren(node *html.Node) []*html.Node {
	var out []*html.Node
	if node == nil {
		return out
	}
	for child := range node.ChildNodes() {
		if child.Type == html.ElementNode {
			out = append(out, child)
		}
	}
	return out
}

// prevElementSibling returns the nearest preceding sibling element, or nil.
func prevElementSibling(node *html.Node) *html.Node {
	if node == nil {
		return nil
	}
	for sibling := node.PrevSibling; sibling != nil; sibling = sibling.PrevSibling {
		if sibling.Type == html.ElementNode {
			return sibling
		}
	}
	return nil
}

// ancestorHasClass reports whether node or any ancestor carries the class.
func ancestorHasClass(node *html.Node, class string) bool {
	for current := node; current != nil; current = current.Parent {
		if nodeHasClass(current, class) {
			return true
		}
	}
	return false
}

// titleLinkNode matches a card's title link: an anchor to its record with
// the js-vim-focus hook the archive's keyboard navigation uses.
func titleLinkNode(node *html.Node) bool {
	return isElement(node, "a") && nodeHasClass(node, "js-vim-focus") && nodeAttr(node, "href") != ""
}

// iconSpanNode matches the icon span marking an author or publisher line,
// whose parent anchor links to a search for that name.
func iconSpanNode(icon string) func(*html.Node) bool {
	return func(node *html.Node) bool {
		if !isElement(node, "span") || !nodeHasClass(node, icon) {
			return false
		}
		parent := node.Parent
		return isElement(parent, "a") && strings.HasPrefix(nodeAttr(parent, "href"), "/search")
	}
}

// metadataLineNode matches the one div of a card or record header carrying
// the metadata line. It has no js- hook, so the styling class is the signal
// and CheckSearchPage notices when it stops matching.
func metadataLineNode(node *html.Node) bool {
	return isElement(node, "div") && nodeHasClass(node, "text-gray-800")
}

// readMoreNode matches the link following a card's description.
func readMoreNode(node *html.Node) bool {
	return isElement(node, "a") && nodeHasClass(node, "js-aarecord-list-read-more")
}
