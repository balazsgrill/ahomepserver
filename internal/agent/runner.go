package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/config"
	"github.com/balazsgrill/ahomepserver/internal/mcp"
	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/google/uuid"
)

// Event represents a real-time progress event emitted during an agent turn.
type Event struct {
	Type      string    `json:"type"` // "status", "tool_call", "tool_result", "assistant_message", "error"
	Name      string    `json:"name,omitempty"`
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
	Content   string    `json:"content,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// EventCallback is invoked whenever the agent loop emits an Event.
type EventCallback func(ev Event)

// Runner executes multi-step LLM + MCP tool loops for a Persona.
type Runner struct {
	tokens     *config.TokenResolver
	httpClient *http.Client
	maxTurns   int
}

// NewRunner creates a new agent Runner.
func NewRunner(tokens *config.TokenResolver, maxTurns int) *Runner {
	if maxTurns <= 0 {
		maxTurns = 10
	}
	return &Runner{
		tokens:     tokens,
		httpClient: &http.Client{Timeout: 90 * time.Second},
		maxTurns:   maxTurns,
	}
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []store.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type chatCompletionRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []store.ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type mcpBinding struct {
	client   *mcp.Client
	toolName string
}

// RunTurn executes a full agent turn (including MCP tool calls) given the conversation history.
// It returns the newly generated messages (assistant tool-call messages, tool result messages, and final assistant message).
func (r *Runner) RunTurn(
	ctx context.Context,
	p *persona.Persona,
	sessionID string,
	history []store.Message,
	onEvent EventCallback,
) ([]store.Message, string, error) {
	emit := func(ev Event) {
		ev.Timestamp = time.Now().UTC()
		if onEvent != nil {
			onEvent(ev)
		}
	}

	// 1. Discover MCP tools and hydrate MCP prompts from all configured MCP servers for this persona
	var tools []openAITool
	var hydratedPrompts []string
	mcpBindings := make(map[string]mcpBinding)

	var latestUserQuery string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" && strings.TrimSpace(history[i].Content) != "" {
			latestUserQuery = strings.TrimSpace(history[i].Content)
			break
		}
	}

	if len(p.Skills) > 0 {
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        "load_skill",
				Description: "Load the full markdown instructions for one of the persona's skills by name.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"skill_name":{"type":"string","description":"Name of the skill to load"}},"required":["skill_name"]}`),
			},
		})
	}

	for _, srvCfg := range p.MCPServers {
		token := r.tokens.Resolve(srvCfg.TokenKey)
		client := mcp.NewClient(srvCfg.Name, srvCfg.URL, srvCfg.Transport, token, srvCfg.Headers)
		emit(Event{
			Type:    "status",
			Name:    srvCfg.Name,
			Content: fmt.Sprintf("Connecting to MCP server %s...", srvCfg.Name),
		})
		srvTools, err := client.ListTools(ctx)
		if err != nil {
			slog.Warn("failed to list tools from MCP server", "persona", p.ID, "mcp_server", srvCfg.Name, "err", err)
			emit(Event{
				Type:    "status",
				Name:    srvCfg.Name,
				Content: fmt.Sprintf("Warning: MCP server %s unavailable (%v)", srvCfg.Name, err),
			})
			continue
		}
		for _, t := range srvTools {
			if !srvCfg.AllowsTool(t.Name) {
				continue
			}
			qName := t.QualifiedName()
			mcpBindings[qName] = mcpBinding{
				client:   client,
				toolName: t.Name,
			}
			if _, exists := mcpBindings[t.Name]; !exists {
				mcpBindings[t.Name] = mcpBinding{
					client:   client,
					toolName: t.Name,
				}
			}
			tools = append(tools, openAITool{
				Type: "function",
				Function: openAIToolFunction{
					Name:        qName,
					Description: fmt.Sprintf("[%s] %s", srvCfg.Name, t.Description),
					Parameters:  t.InputSchema,
				},
			})
		}

		// Discover and retrieve prompts from the MCP server (e.g. core_memories)
		srvPrompts, pErr := client.ListPrompts(ctx)
		if pErr != nil {
			slog.Warn("failed to list prompts from MCP server", "persona", p.ID, "mcp_server", srvCfg.Name, "err", pErr)
		} else {
			for _, pr := range srvPrompts {
				if !srvCfg.AllowsPrompt(pr.Name) {
					continue
				}
				args, ok := resolvePromptArgs(pr, srvCfg.PromptArgs, latestUserQuery)
				if !ok {
					continue
				}
				emit(Event{
					Type:    "status",
					Name:    pr.Name,
					Content: fmt.Sprintf("Hydrating prompt %s from %s...", pr.Name, srvCfg.Name),
				})
				promptText, getErr := client.GetPrompt(ctx, pr.Name, args)
				if getErr != nil {
					slog.Warn("failed to get prompt from MCP server", "persona", p.ID, "mcp_server", srvCfg.Name, "prompt", pr.Name, "err", getErr)
					continue
				}
				if strings.TrimSpace(promptText) != "" {
					hydratedPrompts = append(hydratedPrompts, fmt.Sprintf("## MCP Prompt [%s / %s]\n%s", srvCfg.Name, pr.Name, strings.TrimSpace(promptText)))
				}
			}
		}
	}

	// 2. Build OpenAI messages starting with persona's system prompt + hydrated MCP prompts
	sysPrompt := p.SystemPrompt()
	if len(hydratedPrompts) > 0 {
		sysPrompt = strings.TrimSpace(sysPrompt) + "\n\n# Hydrated Context\n\n" + strings.Join(hydratedPrompts, "\n\n")
	}

	oaMessages := make([]openAIMessage, 0, len(history)+4)
	oaMessages = append(oaMessages, openAIMessage{
		Role:    "system",
		Content: sysPrompt,
	})
	for _, m := range history {
		if m.Role == "system" {
			continue
		}
		oaMessages = append(oaMessages, openAIMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		})
	}

	var generated []store.Message

	// 3. Execute agent loop
	for turn := 0; turn < r.maxTurns; turn++ {
		emit(Event{
			Type:    "status",
			Name:    p.LLM.Model,
			Content: fmt.Sprintf("Calling LLM (%s)...", p.LLM.Model),
		})

		assistantMsg, err := r.callLLM(ctx, p, oaMessages, tools)
		if err != nil {
			emit(Event{
				Type:    "error",
				Content: err.Error(),
			})
			return generated, "", err
		}

		storedAssistant := store.Message{
			ID:        uuid.NewString(),
			SessionID: sessionID,
			Role:      "assistant",
			Content:   assistantMsg.Content,
			ToolCalls: assistantMsg.ToolCalls,
			CreatedAt: time.Now().UTC(),
		}
		generated = append(generated, storedAssistant)
		oaMessages = append(oaMessages, assistantMsg)

		// If no tool calls, the turn is complete
		if len(assistantMsg.ToolCalls) == 0 {
			emit(Event{
				Type:    "assistant_message",
				Content: assistantMsg.Content,
			})
			return generated, assistantMsg.Content, nil
		}

		// Execute each requested tool call
		for _, tc := range assistantMsg.ToolCalls {
			fnName := tc.Function.Name
			argsStr := tc.Function.Arguments
			emit(Event{
				Type:  "tool_call",
				Name:  fnName,
				Input: argsStr,
			})

			toolOutput, toolErr := r.executeTool(ctx, p, mcpBindings, fnName, argsStr)
			if toolErr != nil {
				toolOutput = fmt.Sprintf("Error executing tool %s: %v", fnName, toolErr)
			}

			emit(Event{
				Type:   "tool_result",
				Name:   fnName,
				Input:  argsStr,
				Output: toolOutput,
			})

			toolMsg := store.Message{
				ID:         uuid.NewString(),
				SessionID:  sessionID,
				Role:       "tool",
				Content:    toolOutput,
				ToolCallID: tc.ID,
				CreatedAt:  time.Now().UTC(),
			}
			generated = append(generated, toolMsg)
			oaMessages = append(oaMessages, openAIMessage{
				Role:       "tool",
				Content:    toolOutput,
				ToolCallID: tc.ID,
			})
		}
	}

	err := fmt.Errorf("exceeded maximum agent tool-calling turns (%d)", r.maxTurns)
	emit(Event{Type: "error", Content: err.Error()})
	return generated, "", err
}

func (r *Runner) executeTool(
	ctx context.Context,
	p *persona.Persona,
	bindings map[string]mcpBinding,
	fnName, argsStr string,
) (string, error) {
	var args map[string]interface{}
	if strings.TrimSpace(argsStr) != "" {
		if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments JSON: %w", err)
		}
	}

	if fnName == "load_skill" {
		skillName, _ := args["skill_name"].(string)
		for _, sk := range p.Skills {
			if strings.EqualFold(sk.Name, skillName) {
				return sk.Content, nil
			}
		}
		return "", fmt.Errorf("skill %q not found", skillName)
	}

	binding, ok := bindings[fnName]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", fnName)
	}
	return binding.client.CallTool(ctx, binding.toolName, args)
}

func (r *Runner) callLLM(
	ctx context.Context,
	p *persona.Persona,
	messages []openAIMessage,
	tools []openAITool,
) (openAIMessage, error) {
	endpoint := strings.TrimSpace(p.LLM.Endpoint)
	if endpoint == "" {
		return openAIMessage{}, fmt.Errorf("persona %q has no llm.endpoint configured", p.ID)
	}
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
	}

	reqBody := chatCompletionRequest{
		Model:       p.LLM.Model,
		Messages:    messages,
		Tools:       tools,
		Temperature: p.LLM.Temperature,
		MaxTokens:   p.LLM.MaxTokens,
	}
	rawReq, err := json.Marshal(reqBody)
	if err != nil {
		return openAIMessage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return openAIMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := r.tokens.Resolve(p.LLM.TokenKey); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return openAIMessage{}, fmt.Errorf("LLM request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return openAIMessage{}, fmt.Errorf("reading LLM response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return openAIMessage{}, fmt.Errorf("LLM endpoint HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return openAIMessage{}, fmt.Errorf("decoding LLM response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return openAIMessage{}, fmt.Errorf("LLM error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return openAIMessage{}, fmt.Errorf("LLM returned 0 choices")
	}

	msg := parsed.Choices[0].Message
	return openAIMessage{
		Role:      "assistant",
		Content:   msg.Content,
		ToolCalls: msg.ToolCalls,
	}, nil
}

func resolvePromptArgs(
	pr mcp.Prompt,
	configured map[string]map[string]string,
	latestUserQuery string,
) (map[string]string, bool) {
	args := make(map[string]string)
	if configured != nil {
		if preset, ok := configured[pr.Name]; ok {
			for k, v := range preset {
				args[k] = v
			}
		}
	}
	for _, argDef := range pr.Arguments {
		if _, exists := args[argDef.Name]; exists {
			continue
		}
		if strings.EqualFold(argDef.Name, "query") && latestUserQuery != "" {
			args[argDef.Name] = latestUserQuery
			continue
		}
		if argDef.Required {
			return nil, false
		}
	}
	return args, true
}


