package agent

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// This file implements the HTML-to-markdown engine behind web-fetch: a DOM
// parse, a readability-style main-content extraction pass, and a rule-based
// markdown renderer. Pages where content extraction strips too much fall back
// to converting the whole body, and pages that convert poorly fall back to
// plain text.

// extractedPage is the result of converting a fetched HTML document.
type extractedPage struct {
	Title     string
	Published string
	Markdown  string
}

// render produces the model-facing output: a small front-matter header
// followed by the markdown content.
func (p extractedPage) render(sourceURL string) string {
	var sb strings.Builder
	if p.Title != "" {
		sb.WriteString("Title: ")
		sb.WriteString(p.Title)
		sb.WriteString("\n")
	}
	sb.WriteString("URL Source: ")
	sb.WriteString(sourceURL)
	sb.WriteString("\n")
	if p.Published != "" {
		sb.WriteString("Published Time: ")
		sb.WriteString(p.Published)
		sb.WriteString("\n")
	}
	sb.WriteString("\nMarkdown Content:\n")
	sb.WriteString(p.Markdown)
	return sb.String()
}

// convertHTMLPage parses an HTML document and returns its readable content as
// markdown, plus metadata found in the head.
func convertHTMLPage(src, baseURL string) extractedPage {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return extractedPage{Markdown: htmlFallbackText(src)}
	}
	base, _ := url.Parse(baseURL)
	page := extractedPage{}
	if meta := findNode(doc, atom.Head); meta != nil {
		page.Title, page.Published = extractHeadMetadata(meta)
	}
	body := findNode(doc, atom.Body)
	if body == nil {
		body = doc
	}
	pruneNonContent(body)

	full := renderMarkdown(body, base)
	if candidate := findMainContent(body); candidate != nil {
		extracted := renderMarkdown(candidate, base)
		// Only trust the extraction when it kept a meaningful share of the
		// page; aggressive extraction on already-clean pages loses content.
		if len(extracted) >= int(0.3*float64(len(full))) {
			full = extracted
		}
	}
	if strings.TrimSpace(full) == "" {
		full = htmlFallbackText(src)
	}
	page.Markdown = strings.TrimSpace(full)
	return page
}

// htmlFallbackText is the last-resort conversion: strip all tags and return
// collapsed plain text.
func htmlFallbackText(src string) string {
	s := regexp.MustCompile(`(?is)<(script|style|noscript|head)\b[^>]*>.*?</\s*(script|style|noscript|head)\s*>`).ReplaceAllString(src, " ")
	s = htmlTagRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func findNode(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findNode(c, a); found != nil {
			return found
		}
	}
	return nil
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func extractHeadMetadata(head *html.Node) (title, published string) {
	for c := head.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.Title:
			if c.FirstChild != nil && title == "" {
				title = strings.TrimSpace(c.FirstChild.Data)
			}
		case atom.Meta:
			name := strings.ToLower(attrVal(c, "name") + attrVal(c, "property"))
			content := strings.TrimSpace(attrVal(c, "content"))
			if content == "" {
				continue
			}
			switch name {
			case "og:title":
				if title == "" {
					title = content
				}
			case "article:published_time", "article:modified_time", "date":
				if published == "" {
					published = content
				}
			}
		}
	}
	return title, published
}

// --- non-content pruning -----------------------------------------------

var skippedElements = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Noscript: true,
	atom.Template: true,
	atom.Iframe:   true,
	atom.Object:   true,
	atom.Embed:    true,
	atom.Canvas:   true,
	atom.Svg:      true,
	atom.Form:     true,
	atom.Button:   true,
	atom.Select:   true,
	atom.Input:    true,
	atom.Textarea: true,
	atom.Dialog:   true,
	atom.Head:     true,
}

// pruneNonContent removes elements that never carry readable content, plus
// hidden nodes.
func pruneNonContent(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if c.Type == html.ElementNode {
			hidden := attrVal(c, "hidden") != "" ||
				strings.Contains(strings.ReplaceAll(attrVal(c, "style"), " ", ""), "display:none") ||
				attrVal(c, "aria-hidden") == "true"
			if skippedElements[c.DataAtom] || hidden {
				n.RemoveChild(c)
				continue
			}
		}
		if c.Type == html.CommentNode {
			n.RemoveChild(c)
			continue
		}
		pruneNonContent(c)
	}
}

// --- readability-style content extraction ------------------------------

// emptyQuoteLineRE matches lines holding only blockquote markers, left over
// when a blockquote opens with a block child.
var emptyQuoteLineRE = regexp.MustCompile(`(?m)^(> )+$`)

var (
	positiveHintRE = regexp.MustCompile(`(?i)article|body|content|entry|main|page|post|text|blog|story`)
	negativeHintRE = regexp.MustCompile(`(?i)banner|breadcrumb|combx|comment|community|cookie|disqus|extra|foot|header|legal|menu|modal|related|remark|rss|share|shoutbox|sidebar|skyscraper|social|sponsor|pagination|pager|popup|promo|nav`)
)

// findMainContent scores candidate containers by the text they hold —
// paragraph length, comma count, class/id hints, link density — and returns
// the best one, or nil when no candidate stands out.
func findMainContent(body *html.Node) *html.Node {
	// Fast path: a single semantic <article>/<main> wins outright.
	for _, a := range []atom.Atom{atom.Main, atom.Article} {
		if n := findNode(body, a); n != nil {
			return n
		}
	}
	scores := map[*html.Node]float64{}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.P, atom.Td, atom.Pre, atom.Blockquote:
				text := nodeText(n)
				if len(text) >= 25 {
					points := 1.0 + float64(strings.Count(text, ",")+strings.Count(text, "，")) + min(float64(len(text))/100, 3)
					if p := n.Parent; p != nil {
						scores[p] += points
						if gp := p.Parent; gp != nil {
							scores[gp] += points / 2
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
	var best *html.Node
	bestScore := 0.0
	for n, s := range scores {
		hints := attrVal(n, "class") + " " + attrVal(n, "id")
		if positiveHintRE.MatchString(hints) {
			s += 25
		}
		if negativeHintRE.MatchString(hints) {
			s -= 25
		}
		s *= 1 - linkDensity(n)
		if s > bestScore {
			best, bestScore = n, s
		}
	}
	if bestScore < 20 {
		return nil
	}
	return best
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

// linkDensity is the share of a node's text that lives inside links; high
// density marks navigation and link farms.
func linkDensity(n *html.Node) float64 {
	total := len(nodeText(n))
	if total == 0 {
		return 1
	}
	linked := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.A {
			linked += len(nodeText(n))
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return min(float64(linked)/float64(total), 1)
}

// --- rule-based markdown renderer ---------------------------------------

type mdRenderer struct {
	sb   strings.Builder
	base *url.URL
}

func renderMarkdown(n *html.Node, base *url.URL) string {
	r := &mdRenderer{base: base}
	r.walk(n, mdState{})
	out := r.sb.String()
	out = emptyQuoteLineRE.ReplaceAllString(out, "")
	out = blankLinesRE.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// mdState carries inherited block context down the tree.
type mdState struct {
	listDepth   int
	ordinal     *int // non-nil inside <ol>: shared next item number
	inPre       bool // verbatim text
	quoteDepth  int
	suppressBlk bool // inside inline context (e.g. table cell): no newlines
}

// safeURLSchemes is the allowlist of URL schemes permitted in rendered output.
// Anything else (javascript, data, vbscript, file, etc.) is rejected. Relative
// URLs carry no scheme and are handled separately.
var safeURLSchemes = map[string]bool{
	"http":   true,
	"https":  true,
	"mailto": true,
	"tel":    true,
	"ftp":    true,
}

func (r *mdRenderer) resolveURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Browsers strip TAB, LF and CR from URLs before interpreting the scheme,
	// so attackers use them to hide payloads (e.g. "java\tscript:alert(1)").
	// Remove them before validating so obfuscated schemes can't slip through.
	raw = strings.Map(func(rn rune) rune {
		if rn == '\t' || rn == '\n' || rn == '\r' {
			return -1
		}
		return rn
	}, raw)
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	// A non-empty scheme must be on the allowlist. An empty scheme means a
	// relative reference (path, query or fragment), which is safe to resolve.
	if u.Scheme != "" && !safeURLSchemes[strings.ToLower(u.Scheme)] {
		return ""
	}

	if r.base != nil {
		if resolved := r.base.ResolveReference(u); resolved != nil {
			return resolved.String()
		}
	}
	return u.String()
}

// blockBreak opens a new block, respecting quote prefixes.
func (r *mdRenderer) blockBreak(st mdState) {
	if st.suppressBlk {
		r.sb.WriteString(" ")
		return
	}
	r.sb.WriteString("\n\n")
	if st.quoteDepth > 0 {
		r.sb.WriteString(strings.Repeat("> ", st.quoteDepth))
	}
}

func (r *mdRenderer) walk(n *html.Node, st mdState) {
	switch n.Type {
	case html.TextNode:
		if st.inPre {
			r.sb.WriteString(n.Data)
			return
		}
		text := strings.Join(strings.Fields(n.Data), " ")
		if text == "" {
			return
		}
		// keep natural word spacing between adjacent inline nodes
		if out := r.sb.String(); out != "" && !strings.HasSuffix(out, "\n") && !strings.HasSuffix(out, " ") && !strings.HasSuffix(out, "(") && !strings.HasSuffix(out, "[") {
			r.sb.WriteString(" ")
		}
		r.sb.WriteString(text)
		return
	case html.ElementNode:
		// handled below
	default:
		r.walkChildren(n, st)
		return
	}

	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		r.blockBreak(st)
		r.sb.WriteString(strings.Repeat("#", level))
		r.sb.WriteString(" ")
		r.walkChildren(n, st)
		r.blockBreak(st)
	case atom.P, atom.Div, atom.Section, atom.Article, atom.Header, atom.Footer, atom.Aside, atom.Figure, atom.Figcaption, atom.Details, atom.Summary, atom.Dl, atom.Dt, atom.Dd:
		r.blockBreak(st)
		r.walkChildren(n, st)
	case atom.Br:
		if st.suppressBlk {
			r.sb.WriteString(" ")
		} else {
			r.sb.WriteString("\n")
			if st.quoteDepth > 0 {
				r.sb.WriteString(strings.Repeat("> ", st.quoteDepth))
			}
		}
	case atom.Hr:
		r.blockBreak(st)
		r.sb.WriteString("---")
		r.blockBreak(st)
	case atom.Ul:
		child := st
		child.listDepth++
		child.ordinal = nil
		r.walkChildren(n, child)
	case atom.Ol:
		child := st
		child.listDepth++
		start := 1
		child.ordinal = &start
		r.walkChildren(n, child)
	case atom.Li:
		if st.suppressBlk {
			r.walkChildren(n, st)
			return
		}
		r.sb.WriteString("\n")
		if st.quoteDepth > 0 {
			r.sb.WriteString(strings.Repeat("> ", st.quoteDepth))
		}
		r.sb.WriteString(strings.Repeat("  ", max(st.listDepth-1, 0)))
		if st.ordinal != nil {
			fmt.Fprintf(&r.sb, "%d. ", *st.ordinal)
			*st.ordinal++
		} else {
			r.sb.WriteString("- ")
		}
		r.walkChildren(n, st)
	case atom.Blockquote:
		child := st
		child.quoteDepth++
		r.blockBreak(child)
		r.walkChildren(n, child)
		r.blockBreak(st)
	case atom.Pre:
		if st.suppressBlk {
			r.walkChildren(n, st)
			return
		}
		lang := codeLanguage(n)
		r.blockBreak(st)
		r.sb.WriteString("```")
		r.sb.WriteString(lang)
		r.sb.WriteString("\n")
		child := st
		child.inPre = true
		r.walkChildren(n, child)
		if !strings.HasSuffix(r.sb.String(), "\n") {
			r.sb.WriteString("\n")
		}
		r.sb.WriteString("```")
		r.blockBreak(st)
	case atom.Code, atom.Kbd, atom.Samp:
		if st.inPre {
			r.walkChildren(n, st)
			return
		}
		if out := r.sb.String(); out != "" && !strings.HasSuffix(out, " ") && !strings.HasSuffix(out, "\n") && !strings.HasSuffix(out, "(") && !strings.HasSuffix(out, "[") {
			r.sb.WriteString(" ")
		}
		r.sb.WriteString("`")
		r.sb.WriteString(strings.TrimSpace(nodeText(n)))
		r.sb.WriteString("`")
	case atom.Strong, atom.B:
		r.wrapInline(n, st, "**")
	case atom.Em, atom.I:
		r.wrapInline(n, st, "*")
	case atom.Del, atom.S:
		r.wrapInline(n, st, "~~")
	case atom.A:
		href := r.resolveURL(attrVal(n, "href"))
		label := strings.TrimSpace(nodeText(n))
		if label == "" {
			if alt := r.imageAltInside(n); alt != "" {
				label = alt
			}
		}
		if href == "" || strings.HasPrefix(href, "#") || label == "" {
			r.walkChildren(n, st)
			return
		}
		if out := r.sb.String(); out != "" && !strings.HasSuffix(out, " ") && !strings.HasSuffix(out, "\n") && !strings.HasSuffix(out, "(") {
			r.sb.WriteString(" ")
		}
		r.sb.WriteString("[")
		r.sb.WriteString(label)
		r.sb.WriteString("](")
		r.sb.WriteString(href)
		r.sb.WriteString(")")
	case atom.Img:
		src := r.resolveURL(attrVal(n, "src"))
		alt := strings.TrimSpace(attrVal(n, "alt"))
		if src == "" {
			if alt != "" {
				r.sb.WriteString(" ")
				r.sb.WriteString(alt)
			}
			return
		}
		if out := r.sb.String(); out != "" && !strings.HasSuffix(out, " ") && !strings.HasSuffix(out, "\n") {
			r.sb.WriteString(" ")
		}
		r.sb.WriteString("![")
		r.sb.WriteString(alt)
		r.sb.WriteString("](")
		r.sb.WriteString(src)
		r.sb.WriteString(")")
	case atom.Table:
		if st.suppressBlk {
			r.walkChildren(n, st)
			return
		}
		r.renderTable(n, st)
	default:
		r.walkChildren(n, st)
	}
}

func (r *mdRenderer) walkChildren(n *html.Node, st mdState) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c, st)
		if c.Type == html.ElementNode && (c.DataAtom == atom.Ol || c.DataAtom == atom.Ul) {
			continue
		}
	}
}

func (r *mdRenderer) wrapInline(n *html.Node, st mdState, marker string) {
	text := strings.TrimSpace(nodeText(n))
	if text == "" {
		return
	}
	if out := r.sb.String(); out != "" && !strings.HasSuffix(out, " ") && !strings.HasSuffix(out, "\n") && !strings.HasSuffix(out, "(") && !strings.HasSuffix(out, "[") {
		r.sb.WriteString(" ")
	}
	// links inside emphasis still matter; render children inline for <a>
	if findNode(n, atom.A) != nil {
		r.sb.WriteString(marker)
		r.walkChildren(n, st)
		r.sb.WriteString(marker)
		return
	}
	r.sb.WriteString(marker)
	r.sb.WriteString(strings.Join(strings.Fields(text), " "))
	r.sb.WriteString(marker)
}

func (r *mdRenderer) imageAltInside(n *html.Node) string {
	if img := findNode(n, atom.Img); img != nil {
		return strings.TrimSpace(attrVal(img, "alt"))
	}
	return ""
}

func codeLanguage(pre *html.Node) string {
	classes := attrVal(pre, "class")
	if code := findNode(pre, atom.Code); code != nil {
		classes += " " + attrVal(code, "class")
	}
	for cls := range strings.FieldsSeq(classes) {
		low := strings.ToLower(cls)
		for _, prefix := range []string{"language-", "lang-"} {
			if after, ok := strings.CutPrefix(low, prefix); ok {
				return after
			}
		}
	}
	return ""
}

// renderTable emits a GFM table. Cell content is rendered inline (block
// breaks suppressed) so multi-node cells stay on one row.
func (r *mdRenderer) renderTable(table *html.Node, st mdState) {
	var rows [][]string
	var collect func(n *html.Node)
	collect = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Tr {
			var row []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) {
					cell := &mdRenderer{base: r.base}
					cellState := st
					cellState.suppressBlk = true
					cell.walk(c, cellState)
					text := strings.Join(strings.Fields(cell.sb.String()), " ")
					row = append(row, strings.ReplaceAll(text, "|", "\\|"))
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}
	collect(table)
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, row := range rows {
		width = max(width, len(row))
	}
	r.blockBreak(st)
	for i, row := range rows {
		for len(row) < width {
			row = append(row, "")
		}
		r.sb.WriteString("| ")
		r.sb.WriteString(strings.Join(row, " | "))
		r.sb.WriteString(" |\n")
		if i == 0 {
			r.sb.WriteString("|")
			r.sb.WriteString(strings.Repeat(" --- |", width))
			r.sb.WriteString("\n")
		}
	}
	r.blockBreak(st)
}
