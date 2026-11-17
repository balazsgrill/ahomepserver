package ahp

import (
	"fmt"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func ptr[T any](v T) *T {
	return &v
}

// PersonaToAgentInfo translates a loaded Persona into the standard AHP AgentInfo.
func PersonaToAgentInfo(p *persona.Persona) ahptypes.AgentInfo {
	agent := ahptypes.AgentInfo{
		Provider:    p.ID,
		DisplayName: p.Name,
		Description: p.Description,
		Models: []ahptypes.SessionModelInfo{
			{
				Id:       p.LLM.Model,
				Provider: p.ID,
				Name:     p.LLM.Model,
			},
		},
		Capabilities: &ahptypes.AgentCapabilities{
			MultipleChats: &ahptypes.MultipleChatsCapability{
				Fork:     ptr(false),
				SideChat: ptr(false),
			},
		},
	}

	if len(p.Skills) > 0 {
		var children []ahptypes.ChildCustomization
		for _, sk := range p.Skills {
			desc := sk.Description
			children = append(children, ahptypes.ChildCustomization{
				Value: &ahptypes.SkillCustomization{
					Id:          sk.Name,
					Uri:         fmt.Sprintf("skill://%s/%s", p.ID, sk.Name),
					Name:        sk.Name,
					Description: &desc,
					Type:        ahptypes.CustomizationTypeSkill,
					Enabled:     ptr(true),
				},
			})
		}
		agent.Customizations = append(agent.Customizations, ahptypes.Customization{
			Value: &ahptypes.PluginCustomization{
				Id:       fmt.Sprintf("%s-skills", p.ID),
				Uri:      fmt.Sprintf("plugin://%s/skills", p.ID),
				Name:     fmt.Sprintf("%s Skills", p.Name),
				Type:     ahptypes.CustomizationTypePlugin,
				Children: children,
			},
		})
	}

	for _, mcp := range p.MCPServers {
		agent.Customizations = append(agent.Customizations, ahptypes.Customization{
			Value: &ahptypes.McpServerCustomization{
				Id:   mcp.Name,
				Uri:  mcp.URL,
				Name: mcp.Name,
				Type: ahptypes.CustomizationTypeMcpServer,
				State: ahptypes.McpServerState{
					Value: &ahptypes.McpServerReadyState{
						Kind: ahptypes.McpServerStatusReady,
					},
				},
			},
		})
	}

	return agent
}

// WorkflowToAutomationEntry converts a persona workflow template into a standard AHP AutomationEntry.
func WorkflowToAutomationEntry(personaID string, personaName string, wf persona.WorkflowTemplate) ahptypes.AutomationEntry {
	resURI := fmt.Sprintf("ahp-automation:/%s-%s", personaID, wf.Name)
	now := time.Now().UTC().Format(time.RFC3339)
	return ahptypes.AutomationEntry{
		Resource: resURI,
		Definition: ahptypes.AutomationDefinition{
			Title: fmt.Sprintf("%s: %s", personaName, wf.Name),
			Message: ahptypes.Message{
				Text: wf.PromptTemplate,
				Origin: ahptypes.MessageOrigin{
					Kind: ahptypes.MessageKindAutomation,
				},
			},
			Session: ahptypes.AutomationSessionTemplate{
				Provider: ptr(personaID),
			},
			Enabled:  true,
			Triggers: []ahptypes.AutomationTrigger{},
		},
		Operations: []ahptypes.AutomationOperation{
			ahptypes.AutomationOperation("run"),
		},
		CreatedAt:  now,
		ModifiedAt: now,
	}
}

// MessagesToTurns converts stored session messages into standard AHP Turns.
func MessagesToTurns(messages []store.Message) []ahptypes.Turn {
	var turns []ahptypes.Turn
	var currentTurn *ahptypes.Turn

	for _, m := range messages {
		switch m.Role {
		case "user":
			if currentTurn != nil {
				turns = append(turns, *currentTurn)
			}
			t := m.CreatedAt.UTC().Format(time.RFC3339)
			currentTurn = &ahptypes.Turn{
				Id:        m.ID,
				StartedAt: &t,
				Message: ahptypes.Message{
					Text: m.Content,
					Origin: ahptypes.MessageOrigin{
						Kind: ahptypes.MessageKindUser,
					},
				},
				State: ahptypes.TurnStateComplete,
			}
		case "assistant":
			if currentTurn == nil {
				t := m.CreatedAt.UTC().Format(time.RFC3339)
				currentTurn = &ahptypes.Turn{
					Id:        m.ID,
					StartedAt: &t,
					Message: ahptypes.Message{
						Text: "(Assistant turn)",
						Origin: ahptypes.MessageOrigin{
							Kind: ahptypes.MessageKindAgent,
						},
					},
					State: ahptypes.TurnStateComplete,
				}
			}
			if m.Content != "" {
				currentTurn.ResponseParts = append(currentTurn.ResponseParts, ahptypes.ResponsePart{
					Value: &ahptypes.MarkdownResponsePart{
						Kind:    ahptypes.ResponsePartKindMarkdown,
						Id:      fmt.Sprintf("part-%s", m.ID),
						Content: m.Content,
					},
				})
			}
			for _, tc := range m.ToolCalls {
				currentTurn.ResponseParts = append(currentTurn.ResponseParts, ahptypes.ResponsePart{
					Value: &ahptypes.ToolCallResponsePart{
						Kind: ahptypes.ResponsePartKindToolCall,
						ToolCall: ahptypes.ToolCallState{
							Value: &ahptypes.ToolCallCompletedState{
								ToolCallId:        tc.ID,
								ToolName:          tc.Function.Name,
								DisplayName:       tc.Function.Name,
								InvocationMessage: ahptypes.NewStringOrMarkdownPlain(fmt.Sprintf("Called %s", tc.Function.Name)),
								ToolInput:         &ahptypes.ToolInput{Inline: ptr(tc.Function.Arguments)},
								Success:           true,
								PastTenseMessage:  ahptypes.NewStringOrMarkdownPlain(fmt.Sprintf("Completed %s", tc.Function.Name)),
								Confirmed:         ahptypes.ToolCallConfirmationReasonNotNeeded,
								Status:            ahptypes.ToolCallStatusCompleted,
							},
						},
					},
				})
			}
		case "tool":
			if currentTurn != nil {
				found := false
				for i := range currentTurn.ResponseParts {
					if tp, ok := currentTurn.ResponseParts[i].Value.(*ahptypes.ToolCallResponsePart); ok {
						if tc, ok := tp.ToolCall.Value.(*ahptypes.ToolCallCompletedState); ok && tc.ToolCallId == m.ToolCallID {
							tc.Content = []ahptypes.ToolResultContent{
								{
									Value: &ahptypes.ToolResultTextContent{
										Type: ahptypes.ToolResultContentTypeText,
										Text: m.Content,
									},
								},
							}
							found = true
							break
						}
					}
				}
				if !found {
					currentTurn.ResponseParts = append(currentTurn.ResponseParts, ahptypes.ResponsePart{
						Value: &ahptypes.MarkdownResponsePart{
							Kind:    ahptypes.ResponsePartKindMarkdown,
							Id:      fmt.Sprintf("tool-%s", m.ID),
							Content: fmt.Sprintf("[Tool Result] %s", m.Content),
						},
					})
				}
			}
		}
	}

	if currentTurn != nil {
		turns = append(turns, *currentTurn)
	}
	return turns
}
