package acp

import (
	"context"
	"encoding/json"
	"fmt"

	acpsdk "github.com/coder/acp-go-sdk"
)

// ACP extension methods (names starting with "_") are the protocol's stable
// namespace for functionality outside the core spec. Spettro uses them to
// expose the parts of the TUI that have no core-ACP equivalent — subscription
// login, provider/API-key management, model catalog — so a native client can
// drive the whole product surface instead of telling the user to go run the
// TUI in a terminal.
//
// Everything here resolves against the same config/provider layer the TUI
// uses (config.Update, provider.Manager, internal/spettro), so state written
// by the app is immediately visible to the TUI and vice versa. In particular
// API keys keep living in the encrypted store (~/.spettro/keys.enc) and are
// never sent to the client: the app posts a key in, and reads back only
// whether the provider is connected.
//
// See https://agentclientprotocol.com/protocol/extensibility#extension-methods
const (
	// Account / subscription.
	extAccountStatus      = "_spettro/account/status"
	extAccountLoginStart  = "_spettro/account/login/start"
	extAccountLoginPoll   = "_spettro/account/login/poll"
	extAccountLoginCancel = "_spettro/account/login/cancel"
	extAccountLogout      = "_spettro/account/logout"

	// Providers and models.
	extProvidersList       = "_spettro/providers/list"
	extProvidersConnect    = "_spettro/providers/connect"
	extProvidersDisconnect = "_spettro/providers/disconnect"
	extLocalProbe          = "_spettro/providers/local/probe"
	extLocalAdd            = "_spettro/providers/local/add"
	extLocalRemove         = "_spettro/providers/local/remove"
	extModelsList          = "_spettro/models/list"
	extModelsFavorite      = "_spettro/models/favorite"

	// Agent -> client notifications.
	extAccountUpdate = "_spettro/account/update"
)

// extensionsVersion is bumped whenever the `_spettro/*` surface gains or
// changes methods, so a client can gate features on one number instead of
// probing each method.
const extensionsVersion = 1

// extensionMethods is advertised at initialize (InitializeResponse._meta) so
// clients can feature-detect without a round trip per method.
var extensionMethods = []string{
	extAccountStatus,
	extAccountLoginStart,
	extAccountLoginPoll,
	extAccountLoginCancel,
	extAccountLogout,
	extProvidersList,
	extProvidersConnect,
	extProvidersDisconnect,
	extLocalProbe,
	extLocalAdd,
	extLocalRemove,
	extModelsList,
	extModelsFavorite,
}

var _ acpsdk.ExtensionMethodHandler = (*bridge)(nil)

// HandleExtensionMethod dispatches Spettro's `_spettro/*` extension methods.
// Unknown names get the protocol's method-not-found error, so a client probing
// for a capability this CLI version doesn't have degrades instead of hanging.
func (b *bridge) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case extAccountStatus:
		return b.accountStatus(ctx)
	case extAccountLoginStart:
		return b.accountLoginStart(ctx)
	case extAccountLoginPoll:
		return b.accountLoginPoll()
	case extAccountLoginCancel:
		return b.accountLoginCancel()
	case extAccountLogout:
		return b.accountLogout()

	case extProvidersList:
		return b.providersList()
	case extProvidersConnect:
		return decodeInto(ctx, params, b.providersConnect)
	case extProvidersDisconnect:
		return decodeInto(ctx, params, b.providersDisconnect)
	case extLocalProbe:
		return decodeInto(ctx, params, b.localProbe)
	case extLocalAdd:
		return decodeInto(ctx, params, b.localAdd)
	case extLocalRemove:
		return decodeInto(ctx, params, b.localRemove)
	case extModelsList:
		return b.modelsList()
	case extModelsFavorite:
		return decodeInto(ctx, params, b.modelsFavorite)
	}
	return nil, acpsdk.NewMethodNotFound(method)
}

// decodeInto unmarshals params into the handler's argument type and invokes
// it, turning a malformed payload into an invalid-params error rather than a
// zero-value call that would silently do the wrong thing.
func decodeInto[P any, R any](ctx context.Context, params json.RawMessage, fn func(context.Context, P) (R, error)) (any, error) {
	var p P
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acpsdk.NewInvalidParams(map[string]any{"error": err.Error()})
		}
	}
	return fn(ctx, p)
}

// extError wraps a failure as an ACP internal error carrying a client-facing
// message. Extension handlers use it for expected failures (bad key, endpoint
// unreachable) so the app can show the reason verbatim.
func extError(format string, args ...any) error {
	return &acpsdk.RequestError{Code: -32603, Message: fmt.Sprintf(format, args...)}
}
