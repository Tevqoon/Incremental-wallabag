// Package proofread fans a batch of OCR-mangled passages out to a cheap,
// OpenAI-compatible chat model and asks it to propose mechanical fixes — the
// dropped or duplicated initial letter a scanned book's dropcap produces, a
// wrongly joined or split word, a swapped look-alike character — without
// rewriting anything else about the passage.
//
// The pattern (batched requests, strict JSON back, a soft no-op when
// unconfigured) is carried over from a sibling project's own auto-tagger
// (watch-monitor/tag_llm.py), which talks to the same OpenRouter-compatible
// API with the same deepseek/deepseek-chat default, for the same reason: it
// is inexpensive enough to run over an entire book's worth of passages
// without thinking about the bill.
//
// A leaf package: it knows nothing about elements, documents or the store,
// only "here are some labelled strings, tell me which ones you would
// change and to what". Deciding which passages to send and what to do with
// the answer is the caller's job.
package proofread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultBaseURL and DefaultModel match tag_llm.py's own defaults, kept
	// identical on purpose rather than picked independently: this is the
	// same job (cheap batched text correction against an existing
	// vocabulary/convention) on the same class of provider, and there is no
	// reason for the two projects to have found different good answers to
	// "which cheap model" separately.
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	DefaultModel   = "deepseek/deepseek-chat"

	// BatchSize caps how many passages travel in one request. A book's
	// extract is a paragraph, not a video title, so this is a good deal
	// smaller than tag_llm.py's own batch of 25 titles.
	BatchSize = 10

	requestTimeout = 90 * time.Second
)

// Item is one passage to check, keyed by the caller's own id — increader's
// element id, carried as a string only because a string is what survives a
// round trip through the model's own JSON untouched.
type Item struct {
	ID   string
	Text string
}

// Client calls one OpenAI-compatible /chat/completions endpoint.
type Client struct {
	httpClient *http.Client
	baseURL    string
	model      string
	apiKey     string
}

// NewClient builds a Client. baseURL and model default the same way
// tag_llm.py's own LLM_BASE_URL/LLM_MODEL environment overrides do when
// left empty; apiKey has no default; a Client with an empty apiKey exists
// only so a caller can construct one before checking Configured.
func NewClient(apiKey, baseURL, model string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		httpClient: &http.Client{Timeout: requestTimeout},
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      model,
		apiKey:     apiKey,
	}
}

// Configured reports whether a call would even attempt a request. Proofing
// is an optional enhancement rather than a required step of anything, so
// callers are expected to check this and skip offering the feature at all
// rather than surface a "no key configured" error to the reader.
func (c *Client) Configured() bool { return c.apiKey != "" }

// systemPrompt fixes the class of error rather than any specific text, so
// the same prompt works across books. The dropcap example is spelled out in
// full because it is the one class of OCR error that reads as a content
// change rather than a garbling if described only abstractly — "a
// misplaced letter" does not obviously cover "the word's own first letter
// turns up eight words later".
const systemPrompt = `You are proofreading passages OCR'd from a scanned book. Fix only mechanical scanning errors:
- a large or dropcap initial letter recognised out of order or duplicated elsewhere in the passage — e.g. "NDERSTAND, MY SON, that as long as a man U lacks accomplishments" should become "UNDERSTAND, MY SON, that as long as a man lacks accomplishments": the stray "U" is the dropcap's own first letter, misplaced by the scan, not a word the author wrote there.
- look-alike characters swapped (l/1/I, O/0, rn/m and similar)
- words wrongly joined or split by the scan
- stray characters or line-break artifacts left over from the page layout

Do not do anything else: do not modernise spelling, do not fix grammar the author actually wrote that way, do not paraphrase, do not change punctuation that is not itself a scanning artifact, do not shorten or expand the passage. If a passage has nothing worth fixing, leave its id out of your answer entirely rather than returning it unchanged.

Output strict JSON only: an object mapping each given id to its corrected text. No prose, no markdown fences, no ids you are not changing.`

// FixBatch proposes corrections for items, chunked at BatchSize requests at a
// time. The returned map holds only the ids the model actually proposed a
// change for; any id missing from it was left alone — because the model
// found nothing to fix, or because its batch failed outright. failed counts
// the latter, so a caller can tell "nothing needed fixing" apart from
// "something went wrong" without every passage in a bad batch looking
// individually silent about it.
func (c *Client) FixBatch(ctx context.Context, items []Item) (fixes map[string]string, failed int, err error) {
	fixes = make(map[string]string)
	for start := 0; start < len(items); start += BatchSize {
		end := start + BatchSize
		if end > len(items) {
			end = len(items)
		}
		chunk := items[start:end]

		result, chunkErr := c.fixChunk(ctx, chunk)
		if chunkErr != nil {
			failed += len(chunk)
			continue
		}
		for id, text := range result {
			fixes[id] = text
		}
	}
	if len(items) > 0 && failed == len(items) {
		return fixes, failed, fmt.Errorf("proofread: every batch failed")
	}
	return fixes, failed, nil
}

// fixChunk sends one request, at most BatchSize items.
func (c *Client) fixChunk(ctx context.Context, chunk []Item) (map[string]string, error) {
	ids := make(map[string]bool, len(chunk))
	var user strings.Builder
	payload := make([]itemJSON, len(chunk))
	for i, item := range chunk {
		ids[item.ID] = true
		payload[i] = itemJSON{ID: item.ID, Text: item.Text}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("proofread: encode batch: %w", err)
	}
	user.WriteString("Passages to check, as a JSON array of {id, text}:\n")
	user.Write(encoded)

	content, err := c.chatJSON(ctx, systemPrompt, user.String())
	if err != nil {
		return nil, err
	}
	return parseFixes(content, ids)
}

type itemJSON struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// codeFence strips a ```json ... ``` wrapper some models add despite being
// asked for strict JSON, the same defensive unwrap tag_llm.py's own
// parse_batch_response applies.
var codeFence = regexp.MustCompile("^```[a-zA-Z]*\\n?|```$")

// parseFixes decodes the model's JSON object, keeping only ids that were
// actually part of the batch — a defence against a hallucinated id or a
// model that echoes something outside what it was given, the same
// validation tag_llm.py's own parse_batch_response applies to tags.
func parseFixes(content string, ids map[string]bool) (map[string]string, error) {
	content = strings.TrimSpace(codeFence.ReplaceAllString(strings.TrimSpace(content), ""))

	var decoded map[string]string
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		return nil, fmt.Errorf("proofread: parse model response: %w", err)
	}

	out := make(map[string]string, len(decoded))
	for id, text := range decoded {
		if !ids[id] {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		out[id] = text
	}
	return out, nil
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// chatJSON calls /chat/completions once, retrying without response_format
// on a 400 — some OpenAI-compatible providers reject that field on models
// that do not support it, the same fallback tag_llm.py's own call_llm
// applies.
func (c *Client) chatJSON(ctx context.Context, system, user string) (string, error) {
	request := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature:    0.2,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}

	response, err := c.post(ctx, request)
	if err != nil && strings.Contains(err.Error(), "400") {
		request.ResponseFormat = nil
		response, err = c.post(ctx, request)
	}
	if err != nil {
		return "", err
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("proofread: model returned no choices")
	}
	return response.Choices[0].Message.Content, nil
}

func (c *Client) post(ctx context.Context, payload chatRequest) (chatResponse, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return chatResponse{}, fmt.Errorf("proofread: encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return chatResponse{}, fmt.Errorf("proofread: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")

	raw, err := c.httpClient.Do(request)
	if err != nil {
		return chatResponse{}, fmt.Errorf("proofread: request: %w", err)
	}
	defer raw.Body.Close()

	body, err := io.ReadAll(raw.Body)
	if err != nil {
		return chatResponse{}, fmt.Errorf("proofread: read response: %w", err)
	}
	if raw.StatusCode != http.StatusOK {
		return chatResponse{}, fmt.Errorf("proofread: HTTP %d: %s", raw.StatusCode, truncate(string(body), 400))
	}

	var decoded chatResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return chatResponse{}, fmt.Errorf("proofread: decode response: %w", err)
	}
	return decoded, nil
}

func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}
