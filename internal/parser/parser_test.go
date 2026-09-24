package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParse_Full(t *testing.T) {
	p := New()
	html := loadFixture(t, "full.html")

	result, err := p.Parse("example query", html)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if result.AIOverview == nil {
		t.Fatal("expected AI overview to be extracted")
	}
	if result.AIOverview.Text == "" {
		t.Error("expected non-empty AI overview text")
	}
	if len(result.AIOverview.Sources) != 2 {
		t.Errorf("expected 2 AI overview sources, got %d", len(result.AIOverview.Sources))
	}

	if result.FeaturedSnippet == nil {
		t.Fatal("expected featured snippet to be extracted")
	}
	if result.FeaturedSnippet.Title != "Featured Result Title" {
		t.Errorf("unexpected featured snippet title: %q", result.FeaturedSnippet.Title)
	}

	if len(result.PeopleAlsoAsk) != 3 {
		t.Errorf("expected 3 people-also-ask questions, got %d", len(result.PeopleAlsoAsk))
	}

	if len(result.Organic) != 3 {
		t.Fatalf("expected 3 organic results, got %d", len(result.Organic))
	}
	if result.Organic[0].Position != 1 || result.Organic[0].Title != "First Organic Result" {
		t.Errorf("unexpected first organic result: %+v", result.Organic[0])
	}

	if result.Calibration != nil {
		t.Errorf("expected no calibration note, got %+v", result.Calibration)
	}
}

func TestParse_OrganicOnly(t *testing.T) {
	p := New()
	html := loadFixture(t, "organic_only.html")

	result, err := p.Parse("example query", html)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if result.AIOverview != nil {
		t.Error("expected no AI overview for organic-only fixture")
	}
	if result.FeaturedSnippet != nil {
		t.Error("expected no featured snippet for organic-only fixture")
	}
	if len(result.PeopleAlsoAsk) != 0 {
		t.Error("expected no people-also-ask for organic-only fixture")
	}
	if len(result.Organic) != 2 {
		t.Fatalf("expected 2 organic results, got %d", len(result.Organic))
	}
	if result.Calibration != nil {
		t.Errorf("expected no calibration note when organic results are present, got %+v", result.Calibration)
	}
}

func TestParse_Uncalibrated(t *testing.T) {
	p := New()
	html := loadFixture(t, "uncalibrated.html")

	result, err := p.Parse("example query", html)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if len(result.Organic) != 0 {
		t.Fatalf("expected 0 organic results under shifted layout, got %d", len(result.Organic))
	}
	if result.Calibration == nil {
		t.Fatal("expected a calibration note when organic results are missing")
	}
	found := false
	for _, m := range result.Calibration.MissingBlocks {
		if m == "organic_results" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected organic_results in missing blocks, got %v", result.Calibration.MissingBlocks)
	}
}
