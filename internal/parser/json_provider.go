package parser

import (
	"encoding/json"
	"fmt"

	"github.com/HenryMorganDibie/serp-harvester/internal/model"
)

// providerResponse mirrors the common JSON shape documented by SERP-data API
// providers (field names here match SerpApi's publicly documented response
// schema for organic_results, related_questions, answer_box, and
// ai_overview.text_blocks/references; other providers such as Serper.dev or
// DataForSEO document comparable fields under similar names). Adjust struct
// tags here if a specific vendor's contract differs once one is chosen.
type providerResponse struct {
	OrganicResults []struct {
		Position int    `json:"position"`
		Title    string `json:"title"`
		Link     string `json:"link"`
		Snippet  string `json:"snippet"`
	} `json:"organic_results"`

	RelatedQuestions []struct {
		Question string `json:"question"`
	} `json:"related_questions"`

	AnswerBox *struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"answer_box"`

	AIOverview *struct {
		TextBlocks []struct {
			Type    string `json:"type"`
			Snippet string `json:"snippet"`
		} `json:"text_blocks"`
		References []struct {
			Link string `json:"link"`
		} `json:"references"`
		Error string `json:"error"`
	} `json:"ai_overview"`
}

// JSONParser extracts a model.SerpResult from a third-party SERP provider's
// JSON response instead of raw HTML. The provider has already solved
// consent walls, CAPTCHAs, and proxy rotation as part of their product —
// this just maps their structured output onto the same SerpResult shape the
// HTML Parser produces, so everything downstream (Sink, metrics,
// calibration) is unaffected by which fetch/parse path produced the result.
type JSONParser struct{}

// NewJSON builds a JSONParser.
func NewJSON() *JSONParser {
	return &JSONParser{}
}

// Parse implements the same contract as (*Parser).Parse.
func (p *JSONParser) Parse(query string, body []byte) (*model.SerpResult, error) {
	var raw providerResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("json parser: decode provider response: %w", err)
	}

	result := &model.SerpResult{Query: query}
	var missing []string

	for _, q := range raw.RelatedQuestions {
		if q.Question != "" {
			result.PeopleAlsoAsk = append(result.PeopleAlsoAsk, q.Question)
		}
	}

	if raw.AnswerBox != nil {
		result.FeaturedSnippet = &model.FeaturedSnippet{
			Title: raw.AnswerBox.Title,
			URL:   raw.AnswerBox.Link,
			Text:  raw.AnswerBox.Snippet,
		}
	}

	if raw.AIOverview != nil && raw.AIOverview.Error == "" {
		var text string
		for _, b := range raw.AIOverview.TextBlocks {
			if b.Snippet == "" {
				continue
			}
			if text != "" {
				text += "\n\n"
			}
			text += b.Snippet
		}
		var sources []string
		for _, r := range raw.AIOverview.References {
			if r.Link != "" {
				sources = append(sources, r.Link)
			}
		}
		if text != "" || len(sources) > 0 {
			result.AIOverview = &model.AIOverview{Text: text, Sources: sources}
		}
	}

	for _, r := range raw.OrganicResults {
		result.Organic = append(result.Organic, model.OrganicResult{
			Position: r.Position,
			Title:    r.Title,
			URL:      r.Link,
			Snippet:  r.Snippet,
		})
	}

	if len(result.Organic) == 0 {
		missing = append(missing, "organic_results")
	}
	if len(missing) > 0 {
		result.Calibration = &model.CalibrationNote{MissingBlocks: missing}
	}

	return result, nil
}
