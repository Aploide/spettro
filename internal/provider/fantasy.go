package provider

import (
	"context"
	"strings"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"
	fantasyopenai "charm.land/fantasy/providers/openai"
	fantasyopenaicompat "charm.land/fantasy/providers/openaicompat"

	"spettro/internal/version"
)

func sendWithFantasy(ctx context.Context, providerName, modelName, apiKey, baseURL string, req Request) (Response, error) {
	prov, err := newFantasyProvider(providerName, apiKey, baseURL)
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

	return Response{
		Content:         fantasyText(resp),
		EstimatedTokens: totalTokens,
	}, nil
}

// sendWithFantasyStream is the streaming counterpart of sendWithFantasy. It
// forwards text and reasoning deltas to req.OnStream as they arrive while still
// accumulating the full answer text (reasoning is delivered live but not folded
// into Response.Content, matching the non-streaming path).
func sendWithFantasyStream(ctx context.Context, providerName, modelName, apiKey, baseURL string, req Request) (Response, error) {
	prov, err := newFantasyProvider(providerName, apiKey, baseURL)
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
		EstimatedTokens: totalTokens,
	}, nil
}

// buildFantasyCall assembles the shared fantasy.Call used by both the streaming
// and non-streaming paths.
func buildFantasyCall(providerName string, req Request) fantasy.Call {
	var prompt fantasy.Prompt
	if len(req.Messages) > 0 {
		if req.System != "" {
			prompt = append(prompt, fantasy.NewSystemMessage(req.System))
		}
		for _, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				prompt = append(prompt, fantasy.NewUserMessage(m.Content))
			case RoleAssistant:
				prompt = append(prompt, fantasy.Message{
					Role:    fantasy.MessageRoleAssistant,
					Content: []fantasy.MessagePart{fantasy.TextPart{Text: m.Content}},
				})
			}
		}
	} else {
		prompt = fantasy.Prompt{fantasy.NewUserMessage(req.Prompt)}
	}
	call := fantasy.Call{
		Prompt:    prompt,
		UserAgent: fantasyUserAgent(),
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

func newFantasyProvider(providerName, apiKey, baseURL string) (fantasy.Provider, error) {
	switch providerName {
	case "anthropic":
		opts := []fantasyanthropic.Option{
			fantasyanthropic.WithUserAgent(fantasyUserAgent()),
		}
		if apiKey != "" {
			opts = append(opts, fantasyanthropic.WithAPIKey(apiKey))
		}
		if baseURL != "" {
			opts = append(opts, fantasyanthropic.WithBaseURL(baseURL))
		}
		return fantasyanthropic.New(opts...)
	case "openai":
		opts := []fantasyopenai.Option{
			fantasyopenai.WithUserAgent(fantasyUserAgent()),
			fantasyopenai.WithUseResponsesAPI(),
		}
		if apiKey != "" {
			opts = append(opts, fantasyopenai.WithAPIKey(apiKey))
		}
		if baseURL != "" {
			opts = append(opts, fantasyopenai.WithBaseURL(baseURL))
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
