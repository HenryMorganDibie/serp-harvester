package parser

import "testing"

func TestJSONParser_Parse(t *testing.T) {
	p := NewJSON()
	body := loadFixture(t, "provider_response.json")

	result, err := p.Parse("example query", body)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if len(result.Organic) != 2 {
		t.Fatalf("expected 2 organic results, got %d", len(result.Organic))
	}
	if result.Organic[0].Title != "Provider Organic Result One" {
		t.Errorf("unexpected first organic result: %+v", result.Organic[0])
	}

	if len(result.PeopleAlsoAsk) != 2 {
		t.Errorf("expected 2 related questions, got %d", len(result.PeopleAlsoAsk))
	}

	if result.FeaturedSnippet == nil {
		t.Fatal("expected answer_box to map to FeaturedSnippet")
	}
	if result.FeaturedSnippet.Title != "Provider Answer Box Title" {
		t.Errorf("unexpected featured snippet: %+v", result.FeaturedSnippet)
	}

	if result.AIOverview == nil {
		t.Fatal("expected ai_overview to be extracted")
	}
	if len(result.AIOverview.Sources) != 2 {
		t.Errorf("expected 2 AI overview sources, got %d", len(result.AIOverview.Sources))
	}

	if result.Calibration != nil {
		t.Errorf("expected no calibration note, got %+v", result.Calibration)
	}
}

func TestJSONParser_MissingOrganicIsCalibrated(t *testing.T) {
	p := NewJSON()
	body := []byte(`{"related_questions": [{"question": "only a question, no organic results"}]}`)

	result, err := p.Parse("example query", body)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if result.Calibration == nil {
		t.Fatal("expected calibration note when organic_results is absent")
	}
}

func TestJSONParser_InvalidJSON(t *testing.T) {
	p := NewJSON()
	if _, err := p.Parse("example query", []byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON input")
	}
}
