package provider

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicOption "github.com/anthropics/anthropic-sdk-go/option"
	openai "github.com/openai/openai-go/v3"
	openaiOption "github.com/openai/openai-go/v3/option"
)

// lastUserIndex returns the index of the last plain user turn (the current
// request) in msgs, or -1 if there is none. Tool-result turns are skipped:
// they carry provider tool output, not user content, so attachments never
// belong there.
func lastUserIndex(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser && len(msgs[i].ToolResults) == 0 {
			return i
		}
	}
	return -1
}

// mediaTypeFromPath returns the MIME type for an image file based on extension.
func mediaTypeFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

type OpenAICompatibleAdapter struct {
	APIKey  string
	BaseURL string
}

func (a OpenAICompatibleAdapter) Send(ctx context.Context, model string, req Request) (Response, error) {
	opts := []openaiOption.RequestOption{openaiOption.WithAPIKey(a.APIKey)}
	if a.BaseURL != "" {
		opts = append(opts, openaiOption.WithBaseURL(a.BaseURL))
	}
	client := openai.NewClient(opts...)

	var messages []openai.ChatCompletionMessageParamUnion
	if len(req.Messages) > 0 {
		if req.System != "" {
			messages = append(messages, openai.SystemMessage(req.System))
		}
		// Images belong to the CURRENT turn — the last user message. Attaching
		// them to the first would both bind them to a stale turn and mutate the
		// carried history prefix, breaking provider-side prompt caching.
		imageIdx := lastUserIndex(req.Messages)
		for i, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				imgs := m.Images
				if i == imageIdx {
					imgs = append(imgs[:len(imgs):len(imgs)], req.Images...)
				}
				if len(imgs) > 0 {
					var parts []openai.ChatCompletionContentPartUnionParam
					for _, imgPath := range imgs {
						data, err := os.ReadFile(imgPath)
						if err != nil {
							continue
						}
						mt := mediaTypeFromPath(imgPath)
						dataURL := "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
						parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: dataURL}))
					}
					parts = append(parts, openai.TextContentPart(m.Content))
					messages = append(messages, openai.UserMessage(parts))
				} else {
					messages = append(messages, openai.UserMessage(m.Content))
				}
			case RoleAssistant:
				messages = append(messages, openai.AssistantMessage(m.Content))
			}
		}
	} else if len(req.Images) > 0 {
		var parts []openai.ChatCompletionContentPartUnionParam
		for _, imgPath := range req.Images {
			data, err := os.ReadFile(imgPath)
			if err != nil {
				continue
			}
			mt := mediaTypeFromPath(imgPath)
			dataURL := "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: dataURL,
			}))
		}
		parts = append(parts, openai.TextContentPart(req.Prompt))
		messages = []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(parts),
		}
	} else {
		messages = []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(req.Prompt),
		}
	}

	completion, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    model,
		Messages: messages,
	})
	if err != nil {
		if strings.Contains(err.Error(), "not a chat model") || strings.Contains(err.Error(), "v1/completions") {
			return a.sendLegacyCompletion(ctx, client, model, req)
		}
		return Response{}, err
	}

	content := ""
	if len(completion.Choices) > 0 {
		content = completion.Choices[0].Message.Content
	}
	// OpenAI reports cached tokens as a SUBSET of prompt_tokens; normalize to
	// Anthropic-style split (InputTokens excludes cache reads).
	cached := int(completion.Usage.PromptTokensDetails.CachedTokens)
	return Response{
		Content:         content,
		EstimatedTokens: int(completion.Usage.TotalTokens),
		Usage: Usage{
			InputTokens:     int(completion.Usage.PromptTokens) - cached,
			OutputTokens:    int(completion.Usage.CompletionTokens),
			CacheReadTokens: cached,
		},
	}, nil
}

func (a OpenAICompatibleAdapter) sendLegacyCompletion(ctx context.Context, client openai.Client, model string, req Request) (Response, error) {
	completion, err := client.Completions.New(ctx, openai.CompletionNewParams{
		Model:  openai.CompletionNewParamsModel(model),
		Prompt: openai.CompletionNewParamsPromptUnion{OfString: openai.String(req.Prompt)},
	})
	if err != nil {
		return Response{}, err
	}
	content := ""
	if len(completion.Choices) > 0 {
		content = completion.Choices[0].Text
	}
	return Response{
		Content:         content,
		EstimatedTokens: int(completion.Usage.TotalTokens),
		Usage: Usage{
			InputTokens:  int(completion.Usage.PromptTokens),
			OutputTokens: int(completion.Usage.CompletionTokens),
		},
	}, nil
}

type AnthropicAdapter struct {
	APIKey string
}

func (a AnthropicAdapter) Send(ctx context.Context, model string, req Request) (Response, error) {
	client := anthropic.NewClient(anthropicOption.WithAPIKey(a.APIKey))

	const defaultMaxTokens = int64(16384)
	maxTokens := defaultMaxTokens
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
	}

	if len(req.Messages) > 0 {
		if req.System != "" {
			sysBlock := anthropic.TextBlockParam{Text: req.System}
			sysBlock.CacheControl = anthropic.NewCacheControlEphemeralParam()
			params.System = []anthropic.TextBlockParam{sysBlock}
		}
		// Images attach to the CURRENT turn (last user message), never the
		// first: mutating an already-sent turn would change the cached prefix
		// and force a full prompt-cache miss on every request of the session.
		imageIdx := lastUserIndex(req.Messages)
		var msgs []anthropic.MessageParam
		for i, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				var blocks []anthropic.ContentBlockParamUnion
				imgs := m.Images
				if i == imageIdx {
					imgs = append(imgs[:len(imgs):len(imgs)], req.Images...)
				}
				for _, imgPath := range imgs {
					data, err := os.ReadFile(imgPath)
					if err != nil {
						continue
					}
					mt := mediaTypeFromPath(imgPath)
					blocks = append(blocks, anthropic.NewImageBlockBase64(mt, base64.StdEncoding.EncodeToString(data)))
				}
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
				msgs = append(msgs, anthropic.NewUserMessage(blocks...))
			case RoleAssistant:
				msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
			}
		}
		// Second cache breakpoint on the final message (the system block holds
		// the first): the next request extends this exact prefix, so marking
		// the newest content is what makes the follow-up call a cache read.
		if n := len(msgs); n > 0 {
			final := &msgs[n-1]
			if k := len(final.Content); k > 0 {
				last := &final.Content[k-1]
				if last.OfText != nil {
					last.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
				}
			}
		}
		params.Messages = msgs
	} else {
		// Legacy path: single user message built from Prompt.
		var userBlocks []anthropic.ContentBlockParamUnion
		for _, imgPath := range req.Images {
			data, err := os.ReadFile(imgPath)
			if err != nil {
				continue
			}
			mt := mediaTypeFromPath(imgPath)
			userBlocks = append(userBlocks, anthropic.NewImageBlockBase64(mt, base64.StdEncoding.EncodeToString(data)))
		}
		userBlocks = append(userBlocks, anthropic.NewTextBlock(req.Prompt))
		params.Messages = []anthropic.MessageParam{
			anthropic.NewUserMessage(userBlocks...),
		}
	}

	// Honour the runtime "thinking level" when the model supports extended
	// thinking. Anthropic requires thinking.budget_tokens < max_tokens, so we
	// also bump max_tokens above the budget when needed. Off levels (or empty)
	// are sent as a disabled config to make intent explicit.
	if budget := ThinkingBudgetTokens(ThinkingLevel(req.Thinking)); budget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(int64(budget))
		// Ensure max_tokens > thinking.budget_tokens. Add headroom for the
		// actual answer (4k) on top of the thinking budget.
		needed := int64(budget) + 4096
		if needed > params.MaxTokens {
			params.MaxTokens = needed
		}
	} else if ThinkingLevel(req.Thinking) == ThinkingOff {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		}
	}

	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, err
	}

	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.AsText().Text)
		}
	}
	return Response{
		Content:         sb.String(),
		EstimatedTokens: int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
		Usage: Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
	}, nil
}
