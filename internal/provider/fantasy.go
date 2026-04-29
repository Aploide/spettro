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

	call := fantasy.Call{
		Prompt:    fantasy.Prompt{fantasy.NewUserMessage(req.Prompt)},
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

	resp, err := model.Generate(ctx, call)
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
