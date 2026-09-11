package pagescan

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL and DefaultModel pick a fast, cheap vision model on
	// OpenRouter — the same "pick one good default, let env vars override
	// it" pattern internal/proofread uses for its own text model.
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	DefaultModel   = "google/gemini-3.8-flash"

	// DefaultEffort and MaxTokens exist because of one measurement and one
	// near-miss. At the model's own default thinking level, transcribing a
	// page cost about $0.012 and took 15-23s; at "low" it's about $0.005
	// and 5-10s, with identical output on our test set — so "low" is the
	// default, not a corner someone cut. The near-miss: a cheaper model, at
	// "high" effort, once ran away to 63k thinking tokens transcribing a
	// single page. MaxTokens is the hard backstop against that, and
	// ReadPage treats hitting it (finish_reason "length") as an error
	// rather than returning a truncated transcription.
	DefaultEffort = "low"
	MaxTokens     = 8000

	requestTimeout = 180 * time.Second
)

// Client calls one OpenAI-compatible /chat/completions endpoint with
// OpenRouter's vision and reasoning-effort extensions.
type Client struct {
	httpClient *http.Client
	baseURL    string
	model      string
	effort     string
	apiKey     string
}

// NewClient builds a Client. baseURL, model and effort each default when
// passed "". apiKey has no default; a Client with an empty apiKey exists
// only so a caller can construct one before checking Configured.
func NewClient(apiKey, baseURL, model, effort string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	if effort == "" {
		effort = DefaultEffort
	}
	return &Client{
		httpClient: &http.Client{Timeout: requestTimeout},
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      model,
		effort:     effort,
		apiKey:     apiKey,
	}
}

// Configured reports whether a call would even attempt a request. Like
// proofread's Client, a caller is expected to check this and skip offering
// the feature entirely rather than surface a "no key configured" error to
// the reader.
func (c *Client) Configured() bool { return c.apiKey != "" }

// Model reports the model this Client sends requests to.
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model          string          `json:"model"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	Reasoning      *reasoningOpt   `json:"reasoning,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Messages       []chatMessage   `json:"messages"`
}

// reasoningOpt serialises to whichever of OpenRouter's two reasoning-effort
// shapes applies. Enabled is a pointer so that Enabled: false (an explicit
// "no reasoning") still renders as {"enabled":false} — a plain bool field
// would be indistinguishable from "not set" once it held its zero value.
type reasoningOpt struct {
	Effort  string `json:"effort,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// reasoningFor builds the request's reasoning object for a given effort
// setting: omitted entirely for "", {"enabled":false} for the explicit
// opt-out "none", {"effort":effort} otherwise.
func reasoningFor(effort string) *reasoningOpt {
	switch effort {
	case "":
		return nil
	case "none":
		disabled := false
		return &reasoningOpt{Enabled: &disabled}
	default:
		return &reasoningOpt{Effort: effort}
	}
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ReadPage sends one photo to the vision model and returns its raw JSON
// content, already validated with ParseResult.
//
// Go note: nothing here decodes or resizes image before sending it. HEIC
// and full-resolution JPEG were both verified to reach Gemini through
// OpenRouter unmodified, at the same token count as a downscaled JPEG —
// so there's nothing to gain from spending a dependency (or a CGo image
// library) on doing either in Go.
func (c *Client) ReadPage(ctx context.Context, image []byte, contentType string) (string, error) {
	dataURL := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(image)

	request := chatRequest{
		Model:          c.model,
		Temperature:    0,
		MaxTokens:      MaxTokens,
		Reasoning:      reasoningFor(c.effort),
		ResponseFormat: &responseFormat{Type: "json_object"},
		Messages: []chatMessage{{
			Role: "user",
			Content: []contentPart{
				{Type: "text", Text: prompt},
				{Type: "image_url", ImageURL: &imageURL{URL: dataURL}},
			},
		}},
	}

	response, err := c.post(ctx, request)
	if err != nil && strings.Contains(err.Error(), "400") {
		// Some OpenAI-compatible providers reject response_format on
		// models that don't support it — the same fallback proofread's
		// own chatJSON applies.
		request.ResponseFormat = nil
		response, err = c.post(ctx, request)
	}
	if err != nil {
		return "", err
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("pagescan: model returned no choices")
	}

	choice := response.Choices[0]
	if choice.FinishReason == "length" {
		return "", fmt.Errorf("pagescan: model ran out of tokens before finishing the page (finish_reason=length, max_tokens=%d)", MaxTokens)
	}
	if _, err := ParseResult(choice.Message.Content); err != nil {
		return "", err
	}
	return choice.Message.Content, nil
}

func (c *Client) post(ctx context.Context, payload chatRequest) (chatResponse, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return chatResponse{}, fmt.Errorf("pagescan: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return chatResponse{}, fmt.Errorf("pagescan: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	raw, err := c.httpClient.Do(req)
	if err != nil {
		return chatResponse{}, fmt.Errorf("pagescan: request: %w", err)
	}
	defer raw.Body.Close()

	body, err := io.ReadAll(raw.Body)
	if err != nil {
		return chatResponse{}, fmt.Errorf("pagescan: read response: %w", err)
	}
	if raw.StatusCode != http.StatusOK {
		return chatResponse{}, fmt.Errorf("pagescan: HTTP %d: %s", raw.StatusCode, truncate(string(body), 400))
	}

	var decoded chatResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return chatResponse{}, fmt.Errorf("pagescan: decode response: %w", err)
	}
	return decoded, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// heicBrands and heifBrands are the ISO-BMFF "major brand"/"compatible
// brand" codes that identify a HEIC or HEIF file — see SniffImageType.
var heicBrands = map[string]bool{
	"heic": true, "heix": true, "hevc": true, "heim": true, "heis": true,
	"hevm": true, "hevs": true,
}
var heifBrands = map[string]bool{"mif1": true, "msf1": true}

// SniffImageType returns the MIME type to send the model for image data,
// and whether it's one we accept (image/jpeg, image/png, image/webp,
// image/heic, image/heif). It trusts the bytes over declared, the type the
// caller (an upload's Content-Type header, say) claims: declared is used
// only when the bytes themselves sniff as nothing we recognise.
//
// Go note: HEIC/HEIF share MP4's container format, ISO Base Media File
// Format. Every such file's first box is, after a 4-byte size, the 4-byte
// literal "ftyp" followed by a 4-byte brand code — that's all we read;
// http.DetectContentType doesn't know this format, so it's checked by hand
// here rather than through the stdlib sniffer used for the other three.
func SniffImageType(data []byte, declared string) (string, bool) {
	if mime, ok := sniffHEIF(data); ok {
		return mime, true
	}
	switch detected := http.DetectContentType(data); detected {
	case "image/jpeg", "image/png", "image/webp":
		return detected, true
	default:
		switch declared {
		case "image/jpeg", "image/png", "image/webp", "image/heic", "image/heif":
			return declared, true
		}
		return detected, false
	}
}

func sniffHEIF(data []byte) (string, bool) {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return "", false
	}
	brand := string(data[8:12])
	switch {
	case heicBrands[brand]:
		return "image/heic", true
	case heifBrands[brand]:
		return "image/heif", true
	default:
		return "", false
	}
}
