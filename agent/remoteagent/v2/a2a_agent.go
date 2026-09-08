// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remoteagent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/log"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/auth"
	agentinternal "google.golang.org/adk/v2/internal/agent"
	iremoteagent "google.golang.org/adk/v2/internal/agent/remoteagent"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
)

// BeforeA2ARequestCallback is called before sending a request to the remote agent.
//
// If it returns non-nil result or error, the actual call is skipped and the returned value is used
// as the agent invocation result.
type BeforeA2ARequestCallback func(ctx agent.Context, req *a2a.SendMessageRequest) (*session.Event, error)

// A2AEventConverter can be used to provide a custom implementation of A2A event transformation logic.
type A2AEventConverter func(ctx agent.InvocationContext, req *a2a.SendMessageRequest, event a2a.Event, err error) (*session.Event, error)

// AfterA2ARequestCallback is called after receiving a response from the remote agent and converting it to a session.Event.
// In streaming responses the callback is invoked for every request. Session event parameter might be nil if conversion logic
// decides to not emit an A2A event.
//
// If it returns non-nil result or error, it gets emitted instead of the original result.
type AfterA2ARequestCallback func(ctx agent.Context, req *a2a.SendMessageRequest, resp *session.Event, err error) (*session.Event, error)

// A2ARemoteTaskCleanupCallback is called if Run exited before a terminal event was received from the remote A2A server.
type A2ARemoteTaskCleanupCallback func(ctx context.Context, card *a2a.AgentCard, client A2AClient, taskInfo a2a.TaskInfo, cause error)

// AgentCardProvider resolves an agent card on each agent invocation.
// Use [NewAgentCardProvider] to create a provider from a URL or file path.
// Callers that want lazy/cached resolution should implement caching within the provider function.
type AgentCardProvider func(ctx context.Context) (*a2a.AgentCard, error)

// NewAgentCardProvider creates an [AgentCardProvider] that resolves an agent card from the given source.
// The source can be an http(s) URL or a local file path.
func NewAgentCardProvider(source string, opts ...agentcard.ResolveOption) AgentCardProvider {
	return func(ctx context.Context) (*a2a.AgentCard, error) {
		if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			card, err := agentcard.DefaultResolver.Resolve(ctx, source, opts...)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch an agent card: %w", err)
			}
			return card, nil
		}

		fileBytes, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("failed to read agent card from %q: %w", source, err)
		}

		var card a2a.AgentCard
		if err := json.Unmarshal(fileBytes, &card); err != nil {
			return nil, fmt.Errorf("failed to unmarshal an agent card: %w", err)
		}
		return &card, nil
	}
}

// A2AConfig is used to describe and configure a remote agent.
type A2AConfig struct {
	Name        string
	Description string

	// AgentCard is a static agent card. Either AgentCard or AgentCardProvider must be set.
	AgentCard *a2a.AgentCard
	// AgentCardProvider resolves an agent card on each agent invocation.
	// Use [NewAgentCardProvider] to create a provider from a URL or file path.
	// Either AgentCard or AgentCardProvider must be set.
	AgentCardProvider AgentCardProvider

	// BeforeAgentCallbacks is a list of callbacks that are called sequentially
	// before the agent starts its run.
	//
	// If any callback returns non-nil content or error, then the agent run and
	// the remaining callbacks will be skipped, and a new event will be created
	// from the content or error of that callback.
	BeforeAgentCallbacks []agent.BeforeAgentCallback
	// BeforeRequestCallbacks will be called in the order they are provided until
	// there's a callback that returns a non-nil result or error. Then the
	// actual request is skipped, and the returned response/error is used.
	//
	// This provides an opportunity to inspect, log, or modify the request object.
	// It can also be used to implement caching by returning a cached
	// response, which would skip the actual remote agent call.
	BeforeRequestCallbacks []BeforeA2ARequestCallback
	// Converter is used to convert a2a.Event to session.Event. If not provided, adka2a.ToSessionEvent
	// is used as the default implementation and errors are converted to events with error payload.
	Converter A2AEventConverter
	// AfterRequestCallbacks will be called in the order they are provided until
	// there's a callback that returns a non-nil result or error. Then
	// the actual remote agent event is replaced with the returned result/error.
	//
	// This is the ideal place to log agent responses, collect metrics on token or perform
	// pre-processing of events before a mapper is invoked.
	AfterRequestCallbacks []AfterA2ARequestCallback
	// AfterAgentCallbacks is a list of callbacks that are called sequentially
	// after the agent has completed its run.
	//
	// If any callback returns non-nil content or error, then a new event will be
	// created from the content or error of that callback and the remaining
	// callbacks will be skipped.
	AfterAgentCallbacks []agent.AfterAgentCallback

	// A2APartConverter is a custom converter for converting A2A parts to GenAI parts.
	// Implementations should generally remember to leverage adka2a.ToGenAiPart for default conversions
	// nil returns are considered intentionally dropped parts.
	A2APartConverter adka2a.A2APartConverter

	// GenAIPartConverter is a custom converter for converting GenAI parts to A2A parts.
	// Implementations should generally remember to leverage adka2a.ToA2APart for default conversions
	// nil returns are considered intentionally dropped parts.
	GenAIPartConverter adka2a.GenAIPartConverter

	// ClientProvider can be used to provide a custom implementation of A2A message sending.
	//
	// The context it receives, and the one its client receives per call, carry
	// the credential scope (a2aclient.SessionIDFrom) and remain an
	// agent.InvocationContext. They are a wrapper, though, so an assertion to a
	// type outside that interface no longer succeeds.
	ClientProvider A2AClientProvider

	// Auth, when set, resolves an end-user credential per request and attaches
	// it to the outgoing A2A calls this agent makes whose agent card declares a
	// matching security requirement: the message send, and the CancelTask the
	// run loop issues when it exits before a terminal event. It does not cover
	// the agent card fetch itself, which AgentCardProvider performs before any
	// credential is resolved. It cannot be combined with a custom
	// ClientProvider; set one or the other.
	//
	// Enable Auth only for remote agents whose card comes from a trusted
	// source. The card decides where the request goes and where the credential
	// is written, so an attacker-influenced card can exfiltrate the credential
	// to an endpoint it controls, and a card naming an http:// interface sends
	// it in cleartext. Redirects that leave the card's scheme and host are
	// refused for the same reason.
	//
	// Because the card dictates placement, this is narrower than the field of
	// the same name on mcptoolset.Config, which applies the credential itself:
	//   - Only auth.APIKeyCredential, auth.BearerCredential and
	//     auth.OAuth2Credential can be sent. An auth.BasicCredential or an
	//     auth.WithHeaders value is rejected, since neither survives the
	//     protocol's single-secret handoff.
	//   - APIKeyCredential.Name is ignored: the header name comes from the
	//     card. An OAuth2 token whose type is not bearer is rejected rather
	//     than sent mislabeled, because a2a always writes "Bearer".
	//   - A card scheme a2a cannot place is skipped rather than approximated:
	//     an API key the card wants in a query parameter or a cookie, an HTTP
	//     scheme other than bearer, mutual TLS, and OpenID Connect.
	//   - The OAuth2 scopes a card declares are not honoured, because the a2a
	//     credentials interface never receives them. Scope the provider to
	//     least privilege yourself.
	//
	// Resolution is fail-open: the a2a interceptor logs a resolution error and
	// sends the request unauthenticated — which the remote will likely reject —
	// rather than failing the call. Two consequences follow. An interactive
	// credential cannot work here, because a *auth.ConsentRequiredError is
	// swallowed instead of driving a consent round-trip, so use static tokens,
	// API keys, or 2-legged / service-account sources. And a resolution failure
	// during cleanup sends CancelTask unauthenticated, which a secured remote
	// rejects, leaving the remote task running.
	//
	// The provider is called on every outgoing request, concurrently across
	// concurrent invocations of the same agent, and more than once per request
	// when the card names several security schemes. Its scope identifies both
	// the caller and the callee, so a provider that caches per scope neither
	// crosses users nor sends one remote agent's token to another:
	// a2aclient.SessionIDFrom(ctx) yields the app name, user id, session id and
	// this agent's Name, each percent-encoded and joined with "/".
	//
	// Prefer that scope to the ADK context. On calls this agent makes, ctx is
	// also still the agent.InvocationContext and a provider may type-assert it.
	// A remote agent reached as a subagent of an adka2a-hosted app gets one
	// more call — the cancel that server issues for an abandoned child task —
	// and there the scope is present but the ADK context is not, so a provider
	// that insists on the type assertion fails there and, fail-open, the cancel
	// goes out unauthenticated.
	Auth auth.CredentialProvider

	// MessageSendConfig is attached to a2a.SendMessageRequest sent on every agent invocation.
	MessageSendConfig *a2a.SendMessageConfig

	// RemoteTaskCleanupCallback is called if Run exited before a terminal event was received from the remote A2A server.
	// If Run exited due to an error including context cancellation it will be passed as cause.
	// The context passed to this callback is the original context, but with Err() removed by context.WithoutCancel.
	// If no callback is provided the default behavior is to make a cancel RPC request with 5 second timeout.
	RemoteTaskCleanupCallback A2ARemoteTaskCleanupCallback
}

// NewA2A creates a remote A2A agent. A2A (Agent-To-Agent) protocol is used for communication with an
// agent which can run in a different process or on a different host.
func NewA2A(cfg A2AConfig) (agent.Agent, error) {
	if cfg.AgentCard == nil && cfg.AgentCardProvider == nil {
		return nil, fmt.Errorf("either AgentCard or AgentCardProvider must be provided")
	}
	if isTypedNil(cfg.Auth) {
		return nil, fmt.Errorf("A2AConfig.Auth holds a nil %T; leave the field unset instead", cfg.Auth)
	}
	if cfg.Auth != nil && cfg.ClientProvider != nil {
		return nil, fmt.Errorf("A2AConfig.Auth cannot be combined with a custom ClientProvider; wire the credential into your ClientProvider instead. Its client sees the ADK invocation context on every call, so it can key on remoteagent.CredentialScope(ctx.Session(), name) and attach that with a2aclient.AttachSessionID before delegating")
	}
	if cfg.ClientProvider == nil {
		var opts []a2aclient.FactoryOption
		if cfg.Auth != nil {
			httpClient := authHTTPClient()
			opts = append(opts,
				a2aclient.WithJSONRPCTransport(httpClient),
				a2aclient.WithRESTTransport(httpClient),
				a2aclient.WithCallInterceptors(&a2aclient.AuthInterceptor{
					Service: credentialsService{provider: cfg.Auth, warnMismatch: &sync.Once{}},
				}),
			)
		}
		cfg.ClientProvider = NewA2AClientProvider(a2aclient.NewFactory(opts...))
	}

	remoteAgent := &a2aAgent{
		serverConfig: &iremoteagent.A2AServerConfig{
			AgentCard:         cfg.AgentCard,
			AgentCardProvider: cfg.AgentCardProvider,
			ClientProvider:    cfg.ClientProvider,
			OwnsAuthScope:     cfg.Auth != nil,
		},
	}
	agent, err := agent.New(agent.Config{
		Name:                 cfg.Name,
		Description:          cfg.Description,
		BeforeAgentCallbacks: cfg.BeforeAgentCallbacks,
		AfterAgentCallbacks:  cfg.AfterAgentCallbacks,
		Run: func(ic agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return remoteAgent.run(ic, cfg)
		},
	})
	if err != nil {
		return nil, err
	}

	internalAgent, ok := agent.(agentinternal.Agent)
	if !ok {
		return nil, fmt.Errorf("internal error: failed to convert to internal agent")
	}
	state := agentinternal.Reveal(internalAgent)
	state.AgentType = agentinternal.TypeRemoteAgent
	state.Config = iremoteagent.RemoteAgentState{A2A: remoteAgent.serverConfig}

	return agent, nil
}

type a2aAgent struct {
	serverConfig *iremoteagent.A2AServerConfig
	// warnNoRequirement bounds the "Auth set, card wants none" warning to one
	// per agent rather than one per invocation.
	warnNoRequirement sync.Once
}

func (a *a2aAgent) run(ctx agent.InvocationContext, cfg A2AConfig) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		card, err := iremoteagent.ResolveAgentCard(ctx, a.serverConfig)
		if err != nil {
			yield(toErrorEvent(ctx, fmt.Errorf("agent card resolution failed: %w", err)), nil)
			return
		}

		// Scope every outgoing call of this invocation to the ADK session so
		// the a2a auth interceptor can resolve a credential for it: the message
		// send below, and the cleanup CancelTask the deferred cleanup issues.
		sendCtx := authSendContext(ctx, cfg, card)
		if cfg.Auth != nil && (len(card.SecurityRequirements) == 0 || len(card.SecuritySchemes) == 0) {
			// The interceptor does not even ask for a credential in this case,
			// so the request goes out unauthenticated and nothing else says so:
			// a card that forgot its requirement looks exactly like one that
			// needs no auth. Once per agent — the card is usually static, and
			// the operator needs the fact, not a copy of it per request.
			a.warnNoRequirement.Do(func() {
				log.Warn(ctx, "a2a auth: A2AConfig.Auth is set but the agent card declares no security requirement and scheme pair, so no credential will be attached",
					"agent", cfg.Name)
			})
		}

		sender, err := cfg.ClientProvider(sendCtx, card)
		if err != nil {
			yield(toErrorEvent(ctx, fmt.Errorf("sender creation failed: %w", err)), nil)
			return
		}
		defer destroy(ctx, sender)

		msg, err := newMessage(ctx, cfg)
		if err != nil {
			yield(toErrorEvent(ctx, fmt.Errorf("message creation failed: %w", err)), nil)
			return
		}

		req := &a2a.SendMessageRequest{Message: msg, Config: cfg.MessageSendConfig}
		processor := newRunProcessor(cfg, req)

		if bcbResp, bcbErr := processor.runBeforeA2ARequestCallbacks(ctx); bcbResp != nil || bcbErr != nil {
			if acbResp, acbErr := processor.runAfterA2ARequestCallbacks(ctx, bcbResp, bcbErr); acbResp != nil || acbErr != nil {
				yield(acbResp, acbErr)
			} else {
				yield(bcbResp, bcbErr)
			}
			return
		}

		if len(msg.Parts) == 0 {
			resp := adka2a.NewRemoteAgentEvent(ctx)
			if cbResp, cbErr := processor.runAfterA2ARequestCallbacks(ctx, resp, err); cbResp != nil || cbErr != nil {
				yield(cbResp, cbErr)
			} else {
				yield(resp, nil)
			}
			return
		}

		var lastErr error
		yieldErr := func(err error) bool {
			lastErr = err
			return yield(nil, err)
		}

		var lastEvent a2a.Event
		defer func() {
			err := lastErr
			if err == nil && ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			cleanupRemoteTask(sendCtx, cfg, card, sender, lastEvent, err)
		}()

		processEvent := func(a2aEvent a2a.Event, a2aErr error) bool {
			if a2aEvent != nil {
				lastEvent = a2aEvent
			}

			var err error
			var event *session.Event
			if cfg.Converter != nil {
				event, err = cfg.Converter(ctx, req, a2aEvent, a2aErr)
			} else {
				event, err = processor.convertToSessionEvent(ctx, a2aEvent, a2aErr)
			}

			if cbResp, cbErr := processor.runAfterA2ARequestCallbacks(ctx, event, err); cbResp != nil || cbErr != nil {
				if cbErr != nil {
					return yieldErr(cbErr)
				}
				event = cbResp
				err = nil
			}

			if err != nil {
				return yieldErr(err)
			}

			if event != nil { // an event might be skipped
				for _, toEmit := range processor.aggregatePartial(ctx, a2aEvent, event) {
					if !yield(toEmit, nil) {
						return false
					}
				}
			}
			return true
		}

		if ctx.RunConfig().StreamingMode == agent.StreamingModeNone {
			a2aEvent, a2aErr := sender.SendMessage(sendCtx, req)
			processEvent(a2aEvent, a2aErr)
			return
		}

		for a2aEvent, a2aErr := range sender.SendStreamingMessage(sendCtx, req) {
			if !processEvent(a2aEvent, a2aErr) {
				return
			}
		}
	}
}

// cleanupTimeout bounds the cleanup CancelTask, credential resolution included.
const cleanupTimeout = 5 * time.Second

func cleanupRemoteTask(ctx context.Context, cfg A2AConfig, card *a2a.AgentCard, client A2AClient, lastEvent a2a.Event, cause error) {
	if lastEvent == nil {
		return
	}
	taskID := lastEvent.TaskInfo().TaskID
	if taskID == "" {
		return
	}
	if _, ok := lastEvent.(*a2a.Message); ok {
		return
	}
	var state a2a.TaskState
	if tu, ok := lastEvent.(*a2a.TaskStatusUpdateEvent); ok {
		state = tu.Status.State
	}
	if t, ok := lastEvent.(*a2a.Task); ok {
		state = t.Status.State
	}
	if state.Terminal() {
		return
	}

	// WithoutCancel returns its own type, which is no longer an
	// agent.InvocationContext; re-wrap so a credential provider can still
	// recover the ADK context here, exactly as it can on the send path.
	ctx = reattachInvocation(ctx, context.WithoutCancel(ctx))

	if cfg.RemoteTaskCleanupCallback != nil {
		cfg.RemoteTaskCleanupCallback(ctx, card, client, lastEvent.TaskInfo(), cause)
		return
	}

	if state == a2a.TaskStateInputRequired && cause == nil {
		return
	}
	cancelCtx, cancelTimeout := context.WithTimeout(ctx, cleanupTimeout)
	defer cancelTimeout()
	_, err := client.CancelTask(reattachInvocation(ctx, cancelCtx), &a2a.CancelTaskRequest{ID: taskID})
	if err != nil {
		log.Warn(ctx, "failed to cancel task", "task_id", taskID, "error", err)
	}
}

func newMessage(ctx agent.InvocationContext, cfg A2AConfig) (*a2a.Message, error) {
	events := ctx.Session().Events()
	if userFnCall := getUserFunctionCallAt(events, events.Len()-1); userFnCall != nil {
		event := userFnCall.response
		parts, err := convertParts(ctx, cfg, event)
		if err != nil {
			return nil, fmt.Errorf("event part conversion failed: %w", err)
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, parts...)
		msg.TaskID = a2a.TaskID(userFnCall.taskID)
		msg.ContextID = userFnCall.contextID
		return msg, nil
	}

	parts, contextID := toMissingRemoteSessionParts(ctx, events, cfg)
	msg := a2a.NewMessage(a2a.MessageRoleUser, parts...)
	msg.ContextID = contextID
	return msg, nil
}

func toErrorEvent(ctx agent.InvocationContext, err error) *session.Event {
	event := adka2a.NewRemoteAgentEvent(ctx)
	event.ErrorMessage = err.Error()
	event.CustomMetadata = map[string]any{adka2a.ToADKMetaKey("error"): err.Error()}
	event.TurnComplete = true
	return event
}

func destroy(ctx context.Context, client A2AClient) {
	if err := client.Destroy(); err != nil {
		log.Warn(ctx, "failed to destroy client", "error", err)
	}
}
