package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// Agent questions over ACP.
//
// The ask-user tool lets the model put a decision back to the human. Core ACP
// has no question primitive, so the same structured payload is offered over
// three transports, best available first:
//
//  1. `_spettro/question/ask` (CallExtension) — the full payload and a tagged
//     answer, for clients that advertised the `_spettro/*` surface at
//     handshake;
//  2. `session/request_permission` with the payload mirrored into `_meta`
//     (plus `isRecommended` on the matching option's `_meta`, and a synthetic
//     final option when free text is allowed) — a client that ignores `_meta`
//     still renders a working multiple-choice prompt, which is transport 3;
//  3. `session/elicitation/create` in form mode — the spec's own free-text
//     mechanism, used when the question has no options (or the user picked the
//     free-text option) and the client advertised the capability.
//
// When none of them can reach the user, the model gets a guidance error. It is
// never handed the agent's own default as if a human had chosen it.
const (
	// questionPayloadVersion versions the wire shape. Task 08 extends it with
	// a questions[] array; a client reads this before assuming a layout.
	questionPayloadVersion = 1

	// questionMetaKey carries the outbound question payload on a permission
	// request; questionAnswerMetaKey carries the structured answer back on the
	// response (the request key is also accepted inbound, since a client
	// echoing the key it was given is the likelier mistake than silence).
	questionMetaKey       = "spettro.app/question"
	questionAnswerMetaKey = "spettro.app/questionAnswer"

	// optionRecommendedMetaKey flags the agent's recommended answer on a
	// PermissionOption; optionCustomInputMetaKey flags the synthetic free-text
	// option so a client can render it apart from the agent's own answers.
	optionRecommendedMetaKey = "spettro.app/isRecommended"
	optionCustomInputMetaKey = "spettro.app/isCustomInput"

	// questionCustomOptionID is the synthetic option offered when the question
	// allows free text. It is deliberately not "opt-N": selecting it means
	// "collect text from me", not "this is my answer".
	questionCustomOptionID    = "custom"
	questionCustomOptionLabel = "Type my own answer"

	// elicitationAnswerField is the single property of the elicitation form
	// schema used to collect a free-text answer.
	elicitationAnswerField = "answer"
)

// Answer kinds. declined/cancelled are terminal: the model is told nobody
// answered rather than being given a fabricated choice.
const (
	answerKindOption    = "option"
	answerKindCustom    = "custom"
	answerKindDeclined  = "declined"
	answerKindCancelled = "cancelled"
)

// errQuestionUnreachable is what the model sees when no transport can put the
// question to a human. The wording is guidance the model can act on.
var errQuestionUnreachable = errors.New("this client cannot answer free-form questions; proceed with your best judgment or offer explicit options")

var errQuestionUnanswered = errors.New("user did not answer")

// questionOption is one selectable answer.
type questionOption struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	IsRecommended bool   `json:"isRecommended,omitempty"`
}

// questionPayload is the structured question sent to the client.
type questionPayload struct {
	Version   int    `json:"version"`
	SessionId string `json:"sessionId,omitempty"`
	Question  string `json:"question"`
	Context   string `json:"context,omitempty"`
	// Options is always present (possibly empty) so a client can branch on
	// length without a nil check.
	Options          []questionOption `json:"options"`
	AllowCustomInput bool             `json:"allowCustomInput"`
}

// questionAnswer is the tagged answer shape read back from every transport.
type questionAnswer struct {
	Kind     string `json:"kind"`
	OptionID string `json:"optionId,omitempty"`
	Text     string `json:"text,omitempty"`
}

// questionTransport is the subset of client-facing requests the question flow
// issues. *acpsdk.AgentSideConnection satisfies it; tests substitute a fake so
// each transport can be exercised without a live client.
type questionTransport interface {
	CallExtension(ctx context.Context, method string, params any) (json.RawMessage, error)
	RequestPermission(ctx context.Context, params acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error)
	UnstableCreateElicitation(ctx context.Context, params acpsdk.UnstableCreateElicitationRequest) (acpsdk.UnstableCreateElicitationResponse, error)
}

// transport returns the client request surface, or nil when the bridge has no
// connection (an unconnected bridge must not panic mid-run).
func (b *bridge) transport() questionTransport {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.questionTransport != nil {
		return b.questionTransport
	}
	if b.conn == nil {
		return nil
	}
	return b.conn
}

// clientHasExtension reports whether the client mirrored the `_spettro/*`
// surface back at handshake and listed this method (see Initialize).
func (b *bridge) clientHasExtension(method string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clientExtensions[method]
}

// clientSupportsElicitationForm reports whether the client advertised form
// mode elicitation in its initialize capabilities.
func (b *bridge) clientSupportsElicitationForm() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clientCaps.Elicitation != nil && b.clientCaps.Elicitation.Form != nil
}

// parseClientExtensions reads the extension surface a client advertises in its
// initialize `_meta` (the mirror of what Initialize answers with). Anything
// malformed yields no methods, so the bridge simply falls back to core ACP.
func parseClientExtensions(meta map[string]any) map[string]bool {
	ext, ok := meta[metaExtensionsKey].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := ext["methods"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, entry := range list {
		if name, ok := entry.(string); ok && name != "" {
			out[name] = true
		}
	}
	return out
}

// buildQuestionPayload converts the runtime's request into the wire shape.
// DefaultOption is the recommended marker in this version of the payload
// (task 02 replaces it with a per-option flag on the tool schema itself).
func buildQuestionPayload(sessionID acpsdk.SessionId, ar agent.AskUserRequest) questionPayload {
	def := strings.TrimSpace(ar.DefaultOption)
	options := make([]questionOption, 0, len(ar.Options))
	for i, label := range ar.Options {
		options = append(options, questionOption{
			ID:            fmt.Sprintf("opt-%d", i),
			Label:         label,
			IsRecommended: def != "" && strings.EqualFold(label, def),
		})
	}
	return questionPayload{
		Version:          questionPayloadVersion,
		SessionId:        string(sessionID),
		Question:         strings.TrimSpace(ar.Question),
		Context:          strings.TrimSpace(ar.Context),
		Options:          options,
		AllowCustomInput: ar.AllowFreeResponse || len(options) == 0,
	}
}

// title is the single-line question rendered by clients that show only a tool
// call title (transport 3).
func (p questionPayload) title() string {
	if p.Context == "" {
		return p.Question
	}
	return p.Question + " — " + p.Context
}

// resolve turns a tagged answer into the string handed back to the model.
// Custom text round-trips verbatim; a selected option resolves to its label,
// never to the synthetic free-text option's label.
func (p questionPayload) resolve(a questionAnswer) (string, error) {
	switch a.Kind {
	case answerKindOption:
		for _, o := range p.Options {
			if o.ID == a.OptionID {
				return o.Label, nil
			}
		}
		return "", fmt.Errorf("client selected unknown option %q", a.OptionID)
	case answerKindCustom:
		text := strings.TrimSpace(a.Text)
		if text == "" {
			return "", errQuestionUnanswered
		}
		return text, nil
	case answerKindDeclined, answerKindCancelled, "":
		return "", errQuestionUnanswered
	default:
		return "", fmt.Errorf("client returned unsupported answer kind %q", a.Kind)
	}
}

// askUser puts an agent question to the client over the best transport it
// supports. See the file comment for the ladder.
func (t *turnState) askUser(ctx context.Context, ar agent.AskUserRequest) (string, error) {
	payload := buildQuestionPayload(t.sessionID, ar)
	tr := t.bridge.transport()
	if tr == nil {
		return "", errQuestionUnreachable
	}

	if t.bridge.clientHasExtension(extQuestionAsk) {
		answer, err := askViaExtension(ctx, tr, payload)
		switch {
		case err == nil:
			return payload.resolve(answer)
		case !isMethodNotFound(err):
			return "", err
		}
		// Advertised but not implemented: fall through to core ACP rather
		// than failing a question the client could still render.
	}

	if len(payload.Options) > 0 {
		return t.askViaPermission(ctx, tr, payload)
	}
	if t.bridge.clientSupportsElicitationForm() {
		return askViaElicitation(ctx, tr, payload)
	}
	// No options and no way to collect text. The agent's default is NOT an
	// answer: reporting it would tell the model a human decided something they
	// never saw.
	return "", errQuestionUnreachable
}

// askViaExtension is transport 1: the whole payload, a tagged answer back.
func askViaExtension(ctx context.Context, tr questionTransport, payload questionPayload) (questionAnswer, error) {
	raw, err := tr.CallExtension(ctx, extQuestionAsk, payload)
	if err != nil {
		return questionAnswer{}, err
	}
	var answer questionAnswer
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &answer); err != nil {
			return questionAnswer{}, fmt.Errorf("decode question answer: %w", err)
		}
	}
	return answer, nil
}

// askViaPermission is transport 2 (and, for a client that ignores `_meta`,
// transport 3): the options become permission choices, the payload rides in
// `_meta`, and free text gets a synthetic final option.
func (t *turnState) askViaPermission(ctx context.Context, tr questionTransport, payload questionPayload) (string, error) {
	opts := make([]acpsdk.PermissionOption, 0, len(payload.Options)+1)
	for _, o := range payload.Options {
		opt := acpsdk.PermissionOption{
			OptionId: acpsdk.PermissionOptionId(o.ID),
			Name:     o.Label,
			Kind:     acpsdk.PermissionOptionKindAllowOnce,
		}
		if o.IsRecommended {
			opt.Meta = map[string]any{optionRecommendedMetaKey: true}
		}
		opts = append(opts, opt)
	}
	if payload.AllowCustomInput {
		opts = append(opts, acpsdk.PermissionOption{
			OptionId: questionCustomOptionID,
			Name:     questionCustomOptionLabel,
			Kind:     acpsdk.PermissionOptionKindAllowOnce,
			Meta:     map[string]any{optionCustomInputMetaKey: true},
		})
	}

	title := payload.title()
	resp, err := tr.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: t.sessionID,
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: t.nextToolCallID("ask"),
			Title:      new(title),
			Kind:       acpsdk.Ptr(acpsdk.ToolKindThink),
			Status:     acpsdk.Ptr(acpsdk.ToolCallStatusPending),
		},
		Options: opts,
		Meta:    map[string]any{questionMetaKey: payload},
	})
	if err != nil {
		return "", err
	}

	// A client that understands the payload answers in `_meta`, which is the
	// only way custom text can come back over this transport.
	if answer, ok := answerFromMeta(resp.Meta); ok {
		return payload.resolve(answer)
	}
	if resp.Outcome.Cancelled != nil || resp.Outcome.Selected == nil {
		return "", errQuestionUnanswered
	}
	selected := string(resp.Outcome.Selected.OptionId)
	if selected == questionCustomOptionID {
		// The user asked to type an answer but the client returned no text.
		// Elicitation is the spec's way to collect it; without it there is
		// nothing to report but the gap.
		if t.bridge.clientSupportsElicitationForm() {
			return askViaElicitation(ctx, tr, payload)
		}
		return "", errQuestionUnreachable
	}
	return payload.resolve(questionAnswer{Kind: answerKindOption, OptionID: selected})
}

// askViaElicitation is transport 3 for free text: a one-field JSON-Schema form
// the client renders natively. Gated on ClientCapabilities.Elicitation.Form.
func askViaElicitation(ctx context.Context, tr questionTransport, payload questionPayload) (string, error) {
	title := "Answer"
	description := payload.Context
	req := acpsdk.NewUnstableCreateElicitationRequestForm(acpsdk.UnstableElicitationSchema{
		Title:      &title,
		Type:       "object",
		Properties: map[string]any{elicitationAnswerField: map[string]any{"type": "string", "description": payload.Question}},
		Required:   []string{elicitationAnswerField},
	})
	req.Form.Message = payload.Question
	if description != "" {
		req.Form.RequestedSchema.Description = &description
	}
	req.Form.Meta = map[string]any{questionMetaKey: payload}

	resp, err := tr.UnstableCreateElicitation(ctx, req)
	if err != nil {
		return "", err
	}
	if resp.Accept == nil {
		return "", errQuestionUnanswered
	}
	text, _ := resp.Accept.Content[elicitationAnswerField].(string)
	return payload.resolve(questionAnswer{Kind: answerKindCustom, Text: text})
}

// answerFromMeta reads the structured answer a client attached to a permission
// response. Both the answer key and (leniently) the request key are accepted.
func answerFromMeta(meta map[string]any) (questionAnswer, bool) {
	for _, key := range []string{questionAnswerMetaKey, questionMetaKey} {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var answer questionAnswer
		if err := json.Unmarshal(encoded, &answer); err != nil || answer.Kind == "" {
			continue
		}
		return answer, true
	}
	return questionAnswer{}, false
}

// isMethodNotFound reports whether an error is the JSON-RPC method-not-found
// response, i.e. the client advertised a method it does not actually serve.
func isMethodNotFound(err error) bool {
	var reqErr *acpsdk.RequestError
	return errors.As(err, &reqErr) && reqErr.Code == -32601
}
