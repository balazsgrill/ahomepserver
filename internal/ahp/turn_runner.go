package ahp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/agent"
	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/google/uuid"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func (h *Host) executeChatTurn(ctx context.Context, chatURI string, turnAction *ahptypes.ChatTurnStartedAction) {
	h.mu.RLock()
	sessionURI, ok := h.chatToSession[chatURI]
	var provider string
	if ok {
		if sess, exists := h.sessions[sessionURI]; exists {
			provider = sess.Provider
		}
	}
	h.mu.RUnlock()

	sessionID := strings.TrimPrefix(sessionURI, "ahp-session:/")
	if sessionID == "" {
		sessionID = strings.TrimPrefix(chatURI, "ahp-chat:/")
	}

	p, found := h.personas.Get(provider)
	if !found {
		all := h.personas.List()
		if len(all) > 0 {
			p = all[0]
		}
	}
	if p == nil {
		h.BroadcastAction(chatURI, ahptypes.StateAction{
			Value: &ahptypes.ChatErrorAction{
				Type:     ahptypes.ActionTypeChatError,
				TurnId:   turnAction.TurnId,
				Duration: 0,
				Part: ahptypes.ErrorResponsePart{
					Kind: ahptypes.ResponsePartKindError,
					Error: ahptypes.ErrorInfo{
						Message: fmt.Sprintf("persona/provider %q not found", provider),
					},
				},
			},
		}, nil)
		return
	}

	// 1. Persist user message to store
	now := time.Now().UTC()
	userMsg := store.Message{
		ID:        turnAction.TurnId,
		SessionID: sessionID,
		Role:      "user",
		Content:   turnAction.Message.Text,
		CreatedAt: now,
	}
	_ = h.store.AddMessage(ctx, &userMsg)

	// Fetch conversation history from store
	var history []store.Message
	if storedSess, err := h.store.GetSession(ctx, sessionID); err == nil && storedSess != nil {
		history = storedSess.Messages
	}

	// 2. Open markdown response part for assistant
	partID := fmt.Sprintf("part-%s", uuid.NewString()[:8])
	h.BroadcastAction(chatURI, ahptypes.StateAction{
		Value: &ahptypes.ChatResponsePartAction{
			Type:   ahptypes.ActionTypeChatResponsePart,
			TurnId: turnAction.TurnId,
			Part: ahptypes.ResponsePart{
				Value: &ahptypes.MarkdownResponsePart{
					Kind:    ahptypes.ResponsePartKindMarkdown,
					Id:      partID,
					Content: "",
				},
			},
		},
	}, nil)

	startTime := time.Now()
	var currentToolID string
	var currentToolName string

	onEvent := func(ev agent.Event) {
		switch ev.Type {
		case "assistant_message":
			if ev.Content != "" {
				h.BroadcastAction(chatURI, ahptypes.StateAction{
					Value: &ahptypes.ChatDeltaAction{
						Type:    ahptypes.ActionTypeChatDelta,
						TurnId:  turnAction.TurnId,
						PartId:  partID,
						Content: ev.Content,
					},
				}, nil)
			}
		case "tool_call":
			currentToolID = fmt.Sprintf("tc-%s-%s", ev.Name, uuid.NewString()[:8])
			currentToolName = ev.Name
			h.BroadcastAction(chatURI, ahptypes.StateAction{
				Value: &ahptypes.ChatToolCallStartAction{
					Type:        ahptypes.ActionTypeChatToolCallStart,
					TurnId:      turnAction.TurnId,
					ToolCallId:  currentToolID,
					ToolName:    ev.Name,
					DisplayName: ev.Name,
				},
			}, nil)

			h.BroadcastAction(chatURI, ahptypes.StateAction{
				Value: &ahptypes.ChatToolCallReadyAction{
					Type:              ahptypes.ActionTypeChatToolCallReady,
					TurnId:            turnAction.TurnId,
					ToolCallId:        currentToolID,
					InvocationMessage: ahptypes.NewStringOrMarkdownPlain(fmt.Sprintf("Running tool %s...", ev.Name)),
					ToolInput:         &ahptypes.ToolInput{Inline: ptr(ev.Input)},
				},
			}, nil)

		case "tool_result":
			tcID := currentToolID
			if tcID == "" {
				tcID = fmt.Sprintf("tc-%s-%s", ev.Name, uuid.NewString()[:8])
			}
			h.BroadcastAction(chatURI, ahptypes.StateAction{
				Value: &ahptypes.ChatToolCallCompleteAction{
					Type:       ahptypes.ActionTypeChatToolCallComplete,
					TurnId:     turnAction.TurnId,
					ToolCallId: tcID,
					Result: ahptypes.ToolCallResult{
						Success:          !strings.HasPrefix(ev.Output, "Error"),
						PastTenseMessage: ahptypes.NewStringOrMarkdownPlain(fmt.Sprintf("Completed %s", currentToolName)),
						Content: []ahptypes.ToolResultContent{
							{
								Value: &ahptypes.ToolResultTextContent{
									Type: ahptypes.ToolResultContentTypeText,
									Text: ev.Output,
								},
							},
						},
					},
				},
			}, nil)
			currentToolID = ""
		}
	}

	generated, _, err := h.runner.RunTurn(ctx, p, sessionID, history, onEvent)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		h.BroadcastAction(chatURI, ahptypes.StateAction{
			Value: &ahptypes.ChatErrorAction{
				Type:     ahptypes.ActionTypeChatError,
				TurnId:   turnAction.TurnId,
				Duration: durationMs,
				Part: ahptypes.ErrorResponsePart{
					Kind: ahptypes.ResponsePartKindError,
					Error: ahptypes.ErrorInfo{
						Message: err.Error(),
					},
				},
			},
		}, nil)
		return
	}

	// Persist generated assistant & tool messages to store
	for _, m := range generated {
		_ = h.store.AddMessage(ctx, &m)
	}

	// Broadcast ChatTurnCompleteAction
	h.BroadcastAction(chatURI, ahptypes.StateAction{
		Value: &ahptypes.ChatTurnCompleteAction{
			Type:     ahptypes.ActionTypeChatTurnComplete,
			TurnId:   turnAction.TurnId,
			Duration: durationMs,
		},
	}, nil)

	// Broadcast root/sessionSummaryChanged so session list stays up to date
	h.BroadcastNotification("ahp-root://", "root/sessionSummaryChanged", ahptypes.SessionSummaryChangedParams{
		Channel: "ahp-root://",
		Session: sessionURI,
		Changes: ahptypes.PartialSessionSummary{
			ModifiedAt: ptr(time.Now().UTC().Format(time.RFC3339)),
			Status:     ptr(ahptypes.SessionStatusIdle),
		},
	})
}

// TriggerAutomation starts an automated workflow run, returning its ahp-automation-run:/<id> URI.
func (h *Host) TriggerAutomation(
	ctx context.Context,
	automationURI string,
	inputPayload map[string]any,
	triggerSource string,
) (string, error) {
	h.mu.RLock()
	var targetEntry *ahptypes.AutomationEntry
	for i := range h.autoState.Entries {
		if h.autoState.Entries[i].Resource == automationURI {
			targetEntry = &h.autoState.Entries[i]
			break
		}
	}
	h.mu.RUnlock()

	if targetEntry == nil {
		return "", fmt.Errorf("automation not found: %s", automationURI)
	}

	provider := ""
	if targetEntry.Definition.Session.Provider != nil {
		provider = *targetEntry.Definition.Session.Provider
	}
	p, ok := h.personas.Get(provider)
	if !ok {
		all := h.personas.List()
		if len(all) > 0 {
			p = all[0]
		}
	}
	if p == nil {
		return "", fmt.Errorf("persona %q not found for automation", provider)
	}

	runID := uuid.NewString()
	runURI := fmt.Sprintf("ahp-automation-run:/%s", runID)
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	prompt := targetEntry.Definition.Message.Text
	if len(inputPayload) > 0 {
		payloadJSON, _ := json.MarshalIndent(inputPayload, "", "  ")
		prompt = fmt.Sprintf("%s\n\nIncoming Trigger Payload:\n```json\n%s\n```", strings.TrimSpace(prompt), string(payloadJSON))
	}

	// Create and persist store.WorkflowRun
	stRun := &store.WorkflowRun{
		ID:            runID,
		PersonaID:     p.ID,
		WorkflowName:  targetEntry.Definition.Title,
		TriggerSource: triggerSource,
		InputPayload:  inputPayload,
		Prompt:        prompt,
		Status:        "running",
		CreatedAt:     now,
		StartedAt:     &now,
	}
	_ = h.store.CreateWorkflowRun(ctx, stRun)

	// Create in-memory AutomationRunState
	runState := &ahptypes.AutomationRunState{
		Resource:   runURI,
		Automation: automationURI,
		Origin: ahptypes.AutomationRunOrigin{
			Value: &ahptypes.AutomationManualRunOrigin{
				Kind: "manual",
			},
		},
		Lifecycle: ahptypes.AutomationRunLifecycle{
			Value: &ahptypes.AutomationRunningRunLifecycle{
				Status:    ahptypes.AutomationRunStatusRunning,
				CreatedAt: nowStr,
				StartedAt: nowStr,
			},
		},
		Sessions: []string{},
	}

	h.mu.Lock()
	h.automationRuns[runURI] = runState
	// Add summary to targetEntry.Runs
	summary := ahptypes.AutomationRunSummary{
		Resource:   runURI,
		Automation: automationURI,
		Origin:     runState.Origin,
		Lifecycle:  runState.Lifecycle,
	}
	targetEntry.Runs = append([]ahptypes.AutomationRunSummary{summary}, targetEntry.Runs...)
	h.mu.Unlock()

	// Broadcast automation/set on catalogue
	h.BroadcastAction("ahp-automations://", ahptypes.StateAction{
		Value: &ahptypes.AutomationSetAction{
			Type:       ahptypes.ActionTypeAutomationSet,
			Automation: *targetEntry,
		},
	}, nil)

	// Execute workflow run asynchronously
	go func() {
		runCtx := context.Background()
		var steps []store.WorkflowStep

		onEvent := func(ev agent.Event) {
			stType := "llm_call"
			switch ev.Type {
			case "tool_call":
				stType = "tool_call"
			case "tool_result":
				stType = "tool_result"
			case "error":
				stType = "error"
			}
			steps = append(steps, store.WorkflowStep{
				Timestamp: time.Now().UTC(),
				Type:      stType,
				Name:      ev.Name,
				Input:     ev.Input,
				Output:    ev.Output,
			})
		}

		sessionID := fmt.Sprintf("auto-%s", runID)
		history := []store.Message{
			{
				ID:        uuid.NewString(),
				SessionID: sessionID,
				Role:      "user",
				Content:   prompt,
				CreatedAt: time.Now().UTC(),
			},
		}

		_, output, err := h.runner.RunTurn(runCtx, p, sessionID, history, onEvent)
		completedAt := time.Now().UTC()
		completedAtStr := completedAt.Format(time.RFC3339)

		stRun.CompletedAt = &completedAt
		stRun.Steps = steps

		if err != nil {
			stRun.Status = "failed"
			stRun.Error = err.Error()
			_ = h.store.UpdateWorkflowRun(runCtx, stRun)

			h.BroadcastAction(runURI, ahptypes.StateAction{
				Value: &ahptypes.AutomationRunLifecycleChangedAction{
					Type: ahptypes.ActionTypeAutomationRunLifecycleChanged,
					Lifecycle: ahptypes.AutomationRunLifecycle{
						Value: &ahptypes.AutomationFailedRunLifecycle{
							Status:      ahptypes.AutomationRunStatusFailed,
							CreatedAt:   nowStr,
							StartedAt:   &nowStr,
							CompletedAt: completedAtStr,
							Error: ahptypes.ErrorInfo{
								Message: err.Error(),
							},
						},
					},
				},
			}, nil)
		} else {
			stRun.Status = "completed"
			stRun.Output = output
			_ = h.store.UpdateWorkflowRun(runCtx, stRun)

			h.BroadcastAction(runURI, ahptypes.StateAction{
				Value: &ahptypes.AutomationRunLifecycleChangedAction{
					Type: ahptypes.ActionTypeAutomationRunLifecycleChanged,
					Lifecycle: ahptypes.AutomationRunLifecycle{
						Value: &ahptypes.AutomationCompletedRunLifecycle{
							Status:      ahptypes.AutomationRunStatusCompleted,
							CreatedAt:   nowStr,
							StartedAt:   nowStr,
							CompletedAt: completedAtStr,
						},
					},
				},
			}, nil)

			// Webhook callback if requested
			if stRun.WebhookURL != "" {
				go func(webhookURL, resOutput string) {
					body, _ := json.Marshal(map[string]any{
						"run_id":   runID,
						"status":   "completed",
						"output":   resOutput,
						"finished": completedAtStr,
					})
					req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, webhookURL, bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					resp, postErr := http.DefaultClient.Do(req)
					if postErr != nil {
						slog.Warn("failed to deliver workflow webhook", "url", webhookURL, "err", postErr)
					} else {
						_ = resp.Body.Close()
					}
				}(stRun.WebhookURL, output)
			}
		}
	}()

	return runURI, nil
}
