package extract

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// A synthetic shop category page covering every kind of data Page reads.
const shopPage = `<!doctype html>
<html lang="en-GB">
<head>
  <title>  Kettles |
     Example Shop </title>
  <meta name="Description" content=" Electric kettles in stock. ">
  <link rel="canonical" href="/kettles">
  <meta property="og:title" content="Kettles">
  <meta property="og:image" content="https://cdn.example.test/k.jpg">
  <script type="application/ld+json">{"@type":"ItemList","numberOfItems":2}</script>
  <script type="application/ld+json">{not json</script>
  <style>.x{color:red}</style>
</head>
<body>
  <h1>Kettles</h1>
  <h2>Best sellers</h2>
  <script>var tracking = "not text";</script>
  <ul>
    <li class="product" data-sku="K1">
      <a class="name" href="/p/k1">Steel kettle</a>
      <span class="price">£29.99</span>
      <img src="img/k1.jpg">
    </li>
    <li class="product" data-sku="K2">
      <a class="name" href="https://example.test/p/k2#reviews">Glass kettle</a>
      <span class="price">£34.50</span>
    </li>
  </ul>
  <a href="/p/k1">Steel kettle again</a>
  <a href="mailto:shop@example.test">Mail</a>
  <a href="/login" rel="nofollow">Log in</a>
  <a href="#top">Top</a>
  <nav><a class="next" href="?page=2">Next</a></nav>
</body>
</html>`

func TestPage_GenericData(t *testing.T) {
	p, err := Page("https://example.test/kettles?sort=new", []byte(shopPage), Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Kettles | Example Shop" || p.Description != "Electric kettles in stock." || p.Language != "en-GB" {
		t.Errorf("title=%q description=%q language=%q", p.Title, p.Description, p.Language)
	}
	if p.Canonical != "https://example.test/kettles" {
		t.Errorf("canonical = %q", p.Canonical)
	}
	if len(p.Headings) != 2 || p.Headings[0].Level != 1 || p.Headings[1].Text != "Best sellers" {
		t.Errorf("headings = %+v", p.Headings)
	}
	if p.OpenGraph["og:title"] != "Kettles" || p.OpenGraph["og:image"] != "https://cdn.example.test/k.jpg" {
		t.Errorf("open graph = %v", p.OpenGraph)
	}
	if len(p.JSONLD) != 1 || !json.Valid(p.JSONLD[0]) {
		t.Errorf("json-ld = %s (invalid blocks must be dropped)", p.JSONLD)
	}
	if strings.Contains(p.Text, "tracking") || strings.Contains(p.Text, "color:red") || !strings.Contains(p.Text, "Steel kettle £29.99") {
		t.Errorf("visible text = %q", p.Text)
	}
	wantLinks := []string{
		"https://example.test/p/k1",
		"https://example.test/p/k2",
		"https://example.test/kettles?sort=new", // "#top": this page
		"https://example.test/kettles?page=2",
	}
	if !reflect.DeepEqual(p.Links, wantLinks) {
		t.Errorf("links = %v\nwant %v (absolute, deduplicated, no fragments, mailto or nofollow)", p.Links, wantLinks)
	}
	if p.Extraction != nil || p.Fields != nil || p.Items != nil {
		t.Errorf("no rules, so no fields, items or flags: %+v", p)
	}
}

func TestPage_FieldsAndItems(t *testing.T) {
	spec := Spec{
		Fields: map[string]Rule{
			"heading":    {Selector: "h1", Required: true},
			"next_page":  {Selector: "a.next", Attr: "href"},
			"all_prices": {Selector: ".price", All: true},
			"sku":        {Selector: ".product", Attr: "data-sku"},
		},
		Items: &ItemsSpec{
			Selector: "li.product",
			Required: true,
			Fields: map[string]Rule{
				"name":  {Selector: ".name"},
				"url":   {Selector: ".name", Attr: "href"},
				"price": {Selector: ".price"},
				"image": {Selector: "img", Attr: "src"},
			},
		},
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := Page("https://example.test/kettles", []byte(shopPage), spec)
	if err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]any{
		"heading":    "Kettles",
		"next_page":  "https://example.test/kettles?page=2",
		"all_prices": []string{"£29.99", "£34.50"},
		"sku":        "K1",
	}
	if !reflect.DeepEqual(p.Fields, wantFields) {
		t.Errorf("fields = %#v", p.Fields)
	}
	wantItems := []map[string]any{
		{"name": "Steel kettle", "url": "https://example.test/p/k1", "price": "£29.99", "image": "https://example.test/img/k1.jpg"},
		{"name": "Glass kettle", "url": "https://example.test/p/k2#reviews", "price": "£34.50"},
	}
	if !reflect.DeepEqual(p.Items, wantItems) {
		t.Errorf("items = %#v", p.Items)
	}
	if p.Extraction != nil {
		t.Errorf("nothing required is missing: %+v", p.Extraction)
	}
}

func TestPage_FlagsMissingRequiredData(t *testing.T) {
	spec := Spec{
		Fields: map[string]Rule{
			"price":    {Selector: ".product-price", Required: true},
			"optional": {Selector: ".nowhere"},
		},
		Items: &ItemsSpec{Selector: ".card", Required: true, Fields: map[string]Rule{"name": {Selector: "h3"}}},
	}
	p, err := Page("https://example.test/", []byte(shopPage), spec)
	if err != nil {
		t.Fatal(err)
	}
	if p.Extraction == nil || !reflect.DeepEqual(p.Extraction.Missing, []string{"items", "price"}) {
		t.Errorf("extraction = %+v, want items and price missing (layout drift)", p.Extraction)
	}
}

func TestPage_TextLimitAndBaseHref(t *testing.T) {
	html := `<html><head><base href="https://cdn.example.test/docs/"></head><body><p>héllo wörld</p><a href="a.html">a</a></body></html>`
	p, err := Page("https://example.test/x", []byte(html), Spec{TextLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "h" {
		t.Errorf("text = %q: a 2-byte cap must not split é", p.Text)
	}
	if len(p.Links) != 1 || p.Links[0] != "https://cdn.example.test/docs/a.html" {
		t.Errorf("links = %v: <base href> should apply", p.Links)
	}
	if p, _ := Page("https://example.test/x", []byte(html), Spec{TextLimit: -1, SkipLinks: true}); p.Text != "" || p.Links != nil {
		t.Errorf("text and links should be omitted: %+v", p)
	}
}

func TestSpecValidate_RejectsBadSelectors(t *testing.T) {
	for _, spec := range []Spec{
		{Fields: map[string]Rule{"x": {Selector: "div[unclosed"}}},
		{Fields: map[string]Rule{"x": {Selector: " "}}},
		{Items: &ItemsSpec{Selector: "li", Fields: map[string]Rule{"x": {Selector: "a::"}}}},
	} {
		if err := spec.Validate(); err == nil {
			t.Errorf("%+v: want a validation error", spec)
		}
	}
}

func TestLinks_CustomSelector(t *testing.T) {
	doc, _ := goquery.NewDocumentFromReader(strings.NewReader(shopPage))
	base, _ := url.Parse("https://example.test/kettles")
	if got := Links(base, doc, "li.product a.name"); len(got) != 2 {
		t.Errorf("links = %v", got)
	}
}
