package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"
	fantasyopenai "charm.land/fantasy/providers/openai"
	fantasyopenaicompat "charm.land/fantasy/providers/openaicompat"

	"spettro/internal/models"
	"spettro/internal/version"
)

func sendWithFantasy(ctx context.Context, providerName, apiKind, modelName, apiKey, baseURL string, req Request) (Response, error) {
	prov, err := newFantasyProvider(providerName, apiKind, apiKey, baseURL)
	if err != nil {
		return Response{}, err
	}

	model, err := prov.LanguageModel(ctx, modelName)
	if err != nil {
		return Response{}, err
	}

	resp, err := model.Generate(ctx, buildFantasyCall(providerName, req))
	if err != nil {
		return Response{}, err
	}

	totalTokens := int(resp.Usage.TotalTokens)
	if totalTokens == 0 {
		totalTokens = int(resp.Usage.InputTokens + resp.Usage.OutputTokens)
	}

	var toolCalls []NativeTool
	for _, tc := range resp.Content.ToolCalls() {
		args := json.RawMessage(tc.Input)
		if !json.Valid(args) {
			args = json.RawMessage(`{}`)
		}
		toolCalls = append(toolCalls, NativeTool{ID: tc.ToolCallID, Name: tc.ToolName, Args: args})
	}
	return Response{
		Content:         fantasyText(resp),
		ToolCalls:       toolCalls,
		EstimatedTokens: totalTokens,
		Usage:           usageFromFantasy(resp.Usage),
	}, nil
}

// sendWithFantasyStream is the streaming counterpart of sendWithFantasy. It
// forwards text and reasoning deltas to req.OnStream as they arrive while still
// accumulating the full answer text (reasoning is delivered live but not folded
// into Response.Content, matching the non-streaming path).
func sendWithFantasyStream(ctx context.Context, providerName, apiKind, modelName, apiKey, baseURL string, req Request) (Response, error) {
	prov, err := newFantasyProvider(providerName, apiKind, apiKey, baseURL)
	if err != nil {
		return Response{}, err
	}

	model, err := prov.LanguageModel(ctx, modelName)
	if err != nil {
		return Response{}, err
	}

	stream, err := model.Stream(ctx, buildFantasyCall(providerName, req))
	if err != nil {
		return Response{}, err
	}

	var (
		textSB    strings.Builder
		usage     fantasy.Usage
		streamErr error
		toolCalls []NativeTool
	)
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			textSB.WriteString(part.Delta)
			if req.OnStream != nil && part.Delta != "" {
				req.OnStream(StreamEvent{Kind: StreamText, Delta: part.Delta})
			}
		case fantasy.StreamPartTypeReasoningDelta:
			if req.OnStream != nil && part.Delta != "" {
				req.OnStream(StreamEvent{Kind: StreamReasoning, Delta: part.Delta})
			}
		case fantasy.StreamPartTypeToolCall:
			args := json.RawMessage(part.ToolCallInput)
			if !json.Valid(args) {
				args = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, NativeTool{ID: part.ID, Name: part.ToolCallName, Args: args})
		case fantasy.StreamPartTypeFinish:
			usage = part.Usage
		case fantasy.StreamPartTypeError:
			if part.Error != nil {
				streamErr = part.Error
			}
		}
	}
	if streamErr != nil {
		return Response{}, streamErr
	}

	totalTokens := int(usage.TotalTokens)
	if totalTokens == 0 {
		totalTokens = int(usage.InputTokens + usage.OutputTokens)
	}

	return Response{
		Content:         textSB.String(),
		ToolCalls:       toolCalls,
		EstimatedTokens: totalTokens,
		Usage:           usageFromFantasy(usage),
	}, nil
}

// usageFromFantasy maps fantasy's usage block onto Spettro's Usage type.
func usageFromFantasy(u fantasy.Usage) Usage {
	return Usage{
		InputTokens:      int(u.InputTokens),
		OutputTokens:     int(u.OutputTokens),
		CacheReadTokens:  int(u.CacheReadTokens),
		CacheWriteTokens: int(u.CacheCreationTokens),
	}
}

// buildFantasyCall assembles the shared fantasy.Call used by both the streaming
// and non-streaming paths.
func buildFantasyCall(providerName string, req Request) fantasy.Call {
	var prompt fantasy.Prompt
	if len(req.Messages) > 0 {
		if req.System != "" {
			prompt = append(prompt, fantasy.NewSystemMessage(req.System))
		}
		// Request-level images (legacy field) belong to the current turn — the
		// last plain user message — alongside any message-level images.
		imageIdx := lastUserIndex(req.Messages)
		for i, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				if len(m.ToolResults) > 0 {
					// Tool results must use MessageRoleTool so the Anthropic provider
					// routes them through the tool_result content block path.
					parts := make([]fantasy.MessagePart, 0, len(m.ToolResults))
					// Images produced by tools (screenshot, view-image) ride inside
					// the tool_result as media on Anthropic; other providers only
					// accept text tool results, so their images are re-attached as
					// an immediately following user turn instead.
					var spillImages []string
					for _, tr := range m.ToolResults {
						if providerName == "anthropic" && len(tr.Images) > 0 {
							if media, ok := loadToolResultMedia(tr.Images[0], tr.Output); ok {
								parts = append(parts, fantasy.ToolResultPart{
									ToolCallID: tr.ID,
									Output:     media,
								})
								spillImages = append(spillImages, tr.Images[1:]...)
								continue
							}
						}
						parts = append(parts, fantasy.ToolResultPart{
							ToolCallID: tr.ID,
							Output:     fantasy.ToolResultOutputContentText{Text: tr.Output},
						})
						spillImages = append(spillImages, tr.Images...)
					}
					prompt = append(prompt, fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts})
					// If there is accompanying text, append it as a separate user turn.
					if m.Content != "" {
						prompt = append(prompt, fantasy.NewUserMessage(m.Content))
					}
					if imgs := fantasyImageParts(spillImages); len(imgs) > 0 {
						prompt = append(prompt, fantasy.NewUserMessage("[image attached from the tool result above]", imgs...))
					}
				} else {
					imgs := m.Images
					if i == imageIdx {
						imgs = append(imgs[:len(imgs):len(imgs)], req.Images...)
					}
					prompt = append(prompt, fantasy.NewUserMessage(m.Content, fantasyImageParts(imgs)...))
				}
			case RoleAssistant:
				parts := make([]fantasy.MessagePart, 0, 1+len(m.ToolCalls))
				if m.Content != "" {
					parts = append(parts, fantasy.TextPart{Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					args := string(tc.Args)
					if args == "" {
						args = "{}"
					}
					parts = append(parts, fantasy.ToolCallPart{
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Input:      args,
					})
				}
				if len(parts) == 0 {
					parts = append(parts, fantasy.TextPart{Text: ""})
				}
				prompt = append(prompt, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts})
			}
		}
		if providerName == "anthropic" {
			// Two cache breakpoints: the system prompt and the final message.
			// Marking the FINAL message caches the whole request — including
			// the newest tool results, which are often the largest blocks — so
			// the next call in the loop reads everything before it from cache.
			// (Anthropic looks the prefix up from previously-written
			// breakpoints, so moving the marker forward each step is the
			// intended incremental pattern.)
			cc := anthropicEphemeralOpts()
			prompt[0].ProviderOptions = cc
			if len(prompt) >= 2 {
				prompt[len(prompt)-1].ProviderOptions = cc
			}
		}
	} else {
		prompt = fantasy.Prompt{fantasy.NewUserMessage(req.Prompt, fantasyImageParts(req.Images)...)}
	}
	call := fantasy.Call{
		Prompt:    prompt,
		UserAgent: fantasyUserAgent(),
	}
	if len(req.Tools) > 0 {
		call.Tools = make([]fantasy.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var schema map[string]any
			if err := json.Unmarshal(t.Schema, &schema); err != nil || schema == nil {
				schema = map[string]any{"type": "object", "additionalProperties": true}
			}
			call.Tools = append(call.Tools, fantasy.FunctionTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
		auto := fantasy.ToolChoiceAuto
		call.ToolChoice = &auto
	}
	if req.MaxTokens > 0 {
		maxTokens := int64(req.MaxTokens)
		call.MaxOutputTokens = &maxTokens
	}
	if budget := ThinkingBudgetTokens(ThinkingLevel(req.Thinking)); budget > 0 {
		// Fantasy threads thinking config via per-provider ProviderOptions.
		// Currently we only know how to express it for Anthropic; other
		// providers ignore the field and the fantasy fallback path simply
		// won't include reasoning. This matches Spettro's documented
		// behaviour: thinking levels are honoured by Anthropic, ignored
		// elsewhere.
		if providerName == "anthropic" {
			budgetInt := int64(budget)
			call.ProviderOptions = fantasy.ProviderOptions{
				"anthropic": &fantasyanthropic.ProviderOptions{
					Thinking: &fantasyanthropic.ThinkingProviderOption{BudgetTokens: budgetInt},
				},
			}
			needed := budgetInt + 4096
			if call.MaxOutputTokens == nil || *call.MaxOutputTokens < needed {
				call.MaxOutputTokens = &needed
			}
		}
	}
	return call
}

// loadToolResultMedia reads an image file into a media tool-result output
// (base64 + mime), keeping the tool's text output alongside it. Returns false
// when the file cannot be read so the caller falls back to a text-only result.
func loadToolResultMedia(path, text string) (fantasy.ToolResultOutputContentMedia, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.ToolResultOutputContentMedia{}, false
	}
	return fantasy.ToolResultOutputContentMedia{
		Data:      base64.StdEncoding.EncodeToString(data),
		MediaType: mediaTypeFromPath(path),
		Text:      text,
	}, true
}

// fantasyImageParts loads image files into fantasy FileParts. Unreadable
// paths are skipped (matching the legacy adapters) so a vanished temp file
// degrades to a text-only turn instead of failing the whole request.
func fantasyImageParts(paths []string) []fantasy.FilePart {
	var parts []fantasy.FilePart
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		parts = append(parts, fantasy.FilePart{
			Filename:  filepath.Base(p),
			Data:      data,
			MediaType: mediaTypeFromPath(p),
		})
	}
	return parts
}

func newFantasyProvider(providerName, apiKind, apiKey, baseURL string) (fantasy.Provider, error) {
	switch {
	case providerName == "anthropic" || apiKind == models.APIAnthropic:
		opts := []fantasyanthropic.Option{
			fantasyanthropic.WithUserAgent(fantasyUserAgent()),
		}
		if apiKey != "" {
			opts = append(opts, fantasyanthropic.WithAPIKey(apiKey))
		}
		// The official provider uses the SDK's default endpoint; only
		// anthropic-compatible third parties need an explicit base URL.
		if providerName != "anthropic" && baseURL != "" {
			opts = append(opts, fantasyanthropic.WithBaseURL(baseURL))
		}
		return fantasyanthropic.New(opts...)
	case providerName == "openai":
		opts := []fantasyopenai.Option{
			fantasyopenai.WithUserAgent(fantasyUserAgent()),
			fantasyopenai.WithUseResponsesAPI(),
		}
		if apiKey != "" {
			opts = append(opts, fantasyopenai.WithAPIKey(apiKey))
		}
		return fantasyopenai.New(opts...)
	default:
		resolvedBaseURL, err := resolveOpenAICompatibleBaseURL(providerName, baseURL)
		if err != nil {
			return nil, err
		}
		if apiKey == "" {
			apiKey = "local"
		}

		opts := []fantasyopenaicompat.Option{
			fantasyopenaicompat.WithName(providerName),
			fantasyopenaicompat.WithAPIKey(apiKey),
			fantasyopenaicompat.WithUserAgent(fantasyUserAgent()),
		}
		if resolvedBaseURL != "" {
			opts = append(opts, fantasyopenaicompat.WithBaseURL(resolvedBaseURL))
		}
		if providerName == "openai-compatible" && resolvedBaseURL == "" {
			opts = append(opts, fantasyopenaicompat.WithUseResponsesAPI())
		}
		return fantasyopenaicompat.New(opts...)
	}
}

func shouldFallbackToLegacy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not a chat model") || strings.Contains(msg, "v1/completions")
}

func fantasyText(resp *fantasy.Response) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range resp.Content {
		if text, ok := fantasy.AsContentType[fantasy.TextContent](part); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

func fantasyUserAgent() string {
	return "Spettro/" + version.App + " via fantasy"
}

func anthropicEphemeralOpts() fantasy.ProviderOptions {
	return fantasyanthropic.NewProviderCacheControlOptions(&fantasyanthropic.ProviderCacheControlOptions{
		CacheControl: fantasyanthropic.CacheControl{Type: "ephemeral"},
	})
}
