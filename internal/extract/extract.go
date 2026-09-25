// Package extract turns any HTML page into a model.Page: the generic data
// every page carries (title, meta description, canonical URL, language,
// headings, OpenGraph, JSON-LD, visible text, links) plus fields a target
// defines with CSS selectors, either single values or lists of repeated
// items such as products or listings.
package extract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"

	"github.com/HenryMorganDibie/serp-harvester/internal/model"
)

// Rule extracts one field.
type Rule struct {
	// Selector is a CSS selector, relative to the page or, inside Items,
	// to each item.
	Selector string `yaml:"selector"`
	// Attr is what to read from each match: "" or "text" for its text,
	// "html" for its inner HTML, anything else an attribute name. href,
	// src and action values are resolved to absolute URLs.
	Attr string `yaml:"attr"`
	// All returns every match as a list instead of the first match.
	All bool `yaml:"all"`
	// Required marks the field as expected on every page: if it matches
	// nothing the page is flagged (model.Page.Extraction), meaning the
	// rules no longer fit the page's layout.
	Required bool `yaml:"required"`
}

// ItemsSpec extracts a list of repeated elements, one object each.
type ItemsSpec struct {
	Selector string          `yaml:"selector"`
	Fields   map[string]Rule `yaml:"fields"`
	// Required flags the page if Selector matches nothing.
	Required bool `yaml:"required"`
}

// Spec is a target's extraction rules. The zero value extracts only the
// generic page data.
type Spec struct {
	Fields map[string]Rule `yaml:"fields"`
	Items  *ItemsSpec      `yaml:"items"`
	// TextLimit caps Page.Text in bytes. Zero means DefaultTextLimit;
	// negative omits the text.
	TextLimit int `yaml:"text_limit"`
	// SkipLinks omits Page.Links (links are still used for crawling).
	SkipLinks bool `yaml:"skip_links"`
}

// DefaultTextLimit caps Page.Text when Spec.TextLimit is zero.
const DefaultTextLimit = 64 << 10

// Validate checks every selector in s compiles. goquery silently matches
// nothing for an invalid selector, so a typo would otherwise only show up
// as flagged pages.
func (s Spec) Validate() error {
	check := func(where, sel string) error {
		if strings.TrimSpace(sel) == "" {
			return fmt.Errorf("extract: %s: empty selector", where)
		}
		if _, err := cascadia.Compile(sel); err != nil {
			return fmt.Errorf("extract: %s: invalid selector %q: %w", where, sel, err)
		}
		return nil
	}
	for name, r := range s.Fields {
		if err := check("field "+name, r.Selector); err != nil {
			return err
		}
	}
	if s.Items != nil {
		if err := check("items", s.Items.Selector); err != nil {
			return err
		}
		for name, r := range s.Items.Fields {
			if err := check("items field "+name, r.Selector); err != nil {
				return err
			}
		}
	}
	return nil
}

// Page parses html fetched from pageURL (the final URL, after redirects)
// and extracts the generic page data plus spec's fields and items. Every
// URL in the result is absolute.
func Page(pageURL string, html []byte, spec Spec) (*model.Page, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("extract: page url: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("extract: parse html: %w", err)
	}
	// <base href> changes how the page's relative URLs resolve.
	if href, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if b, err := base.Parse(strings.TrimSpace(href)); err == nil {
			base = b
		}
	}

	p := &model.Page{
		Title:       clean(doc.Find("title").First().Text()),
		Description: metaContent(doc, `meta[name="description" i]`),
		Language:    strings.TrimSpace(doc.Find("html").AttrOr("lang", "")),
	}
	if href, ok := doc.Find(`link[rel="canonical" i]`).First().Attr("href"); ok {
		p.Canonical = resolve(base, href)
	}
	doc.Find("h1, h2, h3").Each(func(_ int, s *goquery.Selection) {
		if text := clean(s.Text()); text != "" {
			p.Headings = append(p.Headings, model.Heading{Level: int(goquery.NodeName(s)[1] - '0'), Text: text})
		}
	})
	doc.Find(`meta[property^="og:"]`).Each(func(_ int, s *goquery.Selection) {
		prop, _ := s.Attr("property")
		if content, ok := s.Attr("content"); ok {
			if p.OpenGraph == nil {
				p.OpenGraph = map[string]string{}
			}
			if _, seen := p.OpenGraph[prop]; !seen {
				p.OpenGraph[prop] = strings.TrimSpace(content)
			}
		}
	})
	doc.Find(`script[type="application/ld+json" i]`).Each(func(_ int, s *goquery.Selection) {
		raw := bytes.TrimSpace([]byte(s.Text()))
		if json.Valid(raw) {
			p.JSONLD = append(p.JSONLD, json.RawMessage(raw))
		}
	})
	if !spec.SkipLinks {
		p.Links = Links(base, doc, "a[href]")
	}
	if spec.TextLimit >= 0 {
		limit := spec.TextLimit
		if limit == 0 {
			limit = DefaultTextLimit
		}
		p.Text = truncateUTF8(visibleText(doc), limit)
	}

	var missing []string
	for name, rule := range spec.Fields {
		v, ok := apply(base, doc.Selection, rule)
		if !ok {
			if rule.Required {
				missing = append(missing, name)
			}
			continue
		}
		if p.Fields == nil {
			p.Fields = map[string]any{}
		}
		p.Fields[name] = v
	}
	if it := spec.Items; it != nil {
		doc.Find(it.Selector).Each(func(_ int, s *goquery.Selection) {
			item := map[string]any{}
			for name, rule := range it.Fields {
				if v, ok := apply(base, s, rule); ok {
					item[name] = v
				}
			}
			if len(item) > 0 {
				p.Items = append(p.Items, item)
			}
		})
		if it.Required && len(p.Items) == 0 {
			missing = append(missing, "items")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		p.Extraction = &model.ExtractionNote{Missing: missing}
	}
	return p, nil
}

// Links returns the absolute, fragment-free http(s) URLs of the elements
// matching selector (their href), in page order without duplicates.
// Links marked rel="nofollow" are skipped.
func Links(base *url.URL, doc *goquery.Document, selector string) []string {
	var out []string
	seen := map[string]bool{}
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		if strings.Contains(strings.ToLower(s.AttrOr("rel", "")), "nofollow") {
			return
		}
		href, ok := s.Attr("href")
		if !ok {
			return
		}
		u, err := base.Parse(strings.TrimSpace(href))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return
		}
		u.Fragment = ""
		u.RawFragment = ""
		if abs := u.String(); !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	})
	return out
}

// apply evaluates rule within s. ok is false when nothing matched.
func apply(base *url.URL, s *goquery.Selection, rule Rule) (any, bool) {
	matches := s.Find(rule.Selector)
	var values []string
	matches.EachWithBreak(func(_ int, m *goquery.Selection) bool {
		if v, ok := read(base, m, rule.Attr); ok {
			values = append(values, v)
		}
		return rule.All
	})
	if len(values) == 0 {
		return nil, false
	}
	if rule.All {
		return values, true
	}
	return values[0], true
}

func read(base *url.URL, m *goquery.Selection, attr string) (string, bool) {
	switch strings.ToLower(attr) {
	case "", "text":
		v := clean(m.Text())
		return v, v != ""
	case "html":
		v, err := m.Html()
		return strings.TrimSpace(v), err == nil && strings.TrimSpace(v) != ""
	}
	v, ok := m.Attr(attr)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	switch strings.ToLower(attr) {
	case "href", "src", "action":
		v = resolve(base, v)
	}
	return v, v != ""
}

func resolve(base *url.URL, ref string) string {
	u, err := base.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return u.String()
}

func metaContent(doc *goquery.Document, selector string) string {
	return strings.TrimSpace(doc.Find(selector).First().AttrOr("content", ""))
}

// visibleText is the body's text without scripts, styles and other
// non-rendered elements, whitespace-collapsed.
func visibleText(doc *goquery.Document) string {
	body := doc.Find("body").Clone()
	body.Find("script, style, noscript, template, svg, iframe").Remove()
	return clean(body.Text())
}

func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
