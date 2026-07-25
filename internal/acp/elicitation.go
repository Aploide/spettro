package acp

// The `elicitation/create` request, spelled out by hand.
//
// Elicitation is how a question that needs typed input — a free-text answer, or
// a whole form of them — reaches a client that can render one. The request the
// Go SDK builds for it is not one a client accepts: the spec flattens an
// elicitation's *scope* (the session it belongs to) into the request body, and
// the SDK's code generator dropped it, generating the scope as a type of its
// own that nothing references. Without `sessionId` the body matches neither
// variant of the client's untagged elicitation mode, so the request is rejected
// as invalid params before the user is shown anything.
//
// So the body below is the spec's, and it goes down the same JSON-RPC
// connection the SDK holds for every other client request. When that connection
// cannot be reached the caller is told elicitation is unavailable, which is a
// question asked some other way rather than a question lost.

import (
	"context"
	"errors"
	"reflect"
	"unsafe"

	acpsdk "github.com/coder/acp-go-sdk"
)

// elicitationModeForm is the mode discriminator for a schema-rendered form.
const elicitationModeForm = "form"

// errElicitationUnavailable means the request never left: the connection could
// not be reached to send it. Callers treat it like a client that does not
// implement elicitation at all.
var errElicitationUnavailable = errors.New("elicitation is unavailable on this connection")

// elicitationRequest is `elicitation/create` in form mode, scoped to a session.
// `sessionId` is the scope the spec flattens in; it is what makes the body
// readable to the client.
type elicitationRequest struct {
	Mode            string                           `json:"mode"`
	SessionId       acpsdk.SessionId                 `json:"sessionId"`
	Message         string                           `json:"message"`
	RequestedSchema acpsdk.UnstableElicitationSchema `json:"requestedSchema"`
	Meta            map[string]any                   `json:"_meta,omitempty"`
}

// newElicitationForm builds a form-mode request for one session.
func newElicitationForm(sessionID acpsdk.SessionId, message string, schema acpsdk.UnstableElicitationSchema) elicitationRequest {
	return elicitationRequest{
		Mode:            elicitationModeForm,
		SessionId:       sessionID,
		Message:         message,
		RequestedSchema: schema,
	}
}

// connTransport is the live client request surface: the SDK's own methods for
// everything it spells correctly, and the hand-built request above for
// elicitation.
type connTransport struct{ *acpsdk.AgentSideConnection }

// CreateElicitation sends the request and decodes the client's answer with the
// SDK's own response type, which reads the accept/decline/cancel shapes fine —
// only the request side is wrong.
func (c connTransport) CreateElicitation(ctx context.Context, params elicitationRequest) (acpsdk.UnstableCreateElicitationResponse, error) {
	conn := sdkConnection(c.AgentSideConnection)
	if conn == nil {
		return acpsdk.UnstableCreateElicitationResponse{}, errElicitationUnavailable
	}
	return acpsdk.SendRequest[acpsdk.UnstableCreateElicitationResponse](conn, ctx, acpsdk.ClientMethodElicitationCreate, params)
}

// sdkConnection reaches the JSON-RPC connection an AgentSideConnection keeps to
// itself. Sending a request the SDK has no method for needs the connection the
// SDK does not hand out, and reading one field is a smaller thing to carry than
// a fork of the SDK.
//
// A nil result means the SDK moved the field, which costs elicitation and
// nothing else: TestSDKConnectionIsReachable fails first, so a dependency bump
// that breaks this is caught here rather than in front of a user.
func sdkConnection(c *acpsdk.AgentSideConnection) *acpsdk.Connection {
	if c == nil {
		return nil
	}
	field := reflect.ValueOf(c).Elem().FieldByName("conn")
	if !field.IsValid() || !field.CanAddr() || field.Type() != reflect.TypeFor[*acpsdk.Connection]() {
		return nil
	}
	conn, _ := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*acpsdk.Connection)
	return conn
}

// elicitationRejected reports whether the client turned the request itself away
// — the method is missing, the body was unreadable, or it never left. A
// rejected elicitation is one the user never saw, so the question is still owed
// them by some other transport; a declined one is not.
func elicitationRejected(err error) bool {
	if errors.Is(err, errElicitationUnavailable) {
		return true
	}
	var reqErr *acpsdk.RequestError
	return errors.As(err, &reqErr) && (reqErr.Code == codeMethodNotFound || reqErr.Code == codeInvalidParams)
}
