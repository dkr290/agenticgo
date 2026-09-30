// Route registration for the Huma API. Every REST endpoint is a
// huma.Register call here, so the OpenAPI spec (/openapi.json) and the
// generated docs (/docs) always match the served API. Non-REST surface —
// the /ws WebSocket chat and the embedded SPA — stays on the plain mux.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/dkr290/agenticgo/internal/agent"
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/providers"
	"github.com/dkr290/agenticgo/internal/scaffold"
	"github.com/dkr290/agenticgo/internal/skills"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
)

// registerRoutes declares every REST operation on the huma API.
func (s *Server) registerRoutes(api huma.API) {
	// --- Health ---

	huma.Register(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Health check",
		Tags:        []string{"Health"},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		out := &healthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	s.registerAgentRoutes(api)
	s.registerChatRoutes(api)
	s.registerContextFileRoutes(api)
	s.registerSkillRoutes(api)
	s.registerImageRoutes(api)
	s.registerEvolveRoutes(api)
	s.registerKnowledgeRoutes(api)
	s.registerObservationRoutes(api)
	s.registerDocRoutes(api)
	s.registerSessionRoutes(api)
	s.registerToolRoutes(api)
	s.registerProviderRoutes(api)
	s.registerMCPRoutes(api)
	s.registerCronRoutes(api)
}

// --- Agent CRUD ---

func (s *Server) registerAgentRoutes(api huma.API) {
	tag := []string{"Agents"}

	huma.Register(api, huma.Operation{
		OperationID: "list-agents",
		Method:      http.MethodGet,
		Path:        "/api/agents",
		Summary:     "List agents",
		Description: "Lists every agent registered under the agents directory.",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []agents.Agent }, error) {
		list, err := s.agents.List()
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if list == nil {
			list = []agents.Agent{}
		}
		return &struct{ Body []agents.Agent }{Body: list}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-agent",
		Method:        http.MethodPost,
		Path:          "/api/agents",
		Summary:       "Create an agent",
		Description:   "Creates an agent directory seeded from the embedded context-file templates.",
		Tags:          tag,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, input *createAgentInput) (*struct{ Body *agents.Agent }, error) {
		var cfg agents.AgentConfig
		if input.Body.Config != nil {
			cfg = *input.Body.Config
		}
		ag, err := s.agents.Create(input.Body.Key, input.Body.Name, input.Body.Description, input.Body.Soul, cfg)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body *agents.Agent }{Body: ag}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-agent",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}",
		Summary:     "Get an agent",
		Description: "Returns one agent with its context files and config.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body *agents.Agent }, error) {
		ag, err := s.agents.Get(input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &struct{ Body *agents.Agent }{Body: ag}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-agent",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}",
		Summary:     "Delete an agent",
		Description: "Deletes an agent directory including its workspace and images.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*statusOutput, error) {
		if err := s.agents.Delete(input.Key); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-agent-config",
		Method:      http.MethodPut,
		Path:        "/api/agents/{key}/config",
		Summary:     "Replace agent config",
		Description: "Replaces an agent's per-agent LLM config (provider/model/temperature/max_tokens/vision + enabled skills/tools/commands).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key  string `path:"key" doc:"Agent key"`
		Body agents.AgentConfig
	}) (*struct{ Body *agents.Agent }, error) {
		ag, err := s.agents.UpdateConfig(input.Key, input.Body)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &struct{ Body *agents.Agent }{Body: ag}, nil
	})
}

// --- Chat (non-streaming REST) ---

// registerChatRoutes adds POST /api/chat: one synchronous agent turn over
// plain HTTP — the same Engine.Run the WebSocket /ws endpoint streams, but
// with events collected and returned in one response. This is the endpoint
// for curl/scripts/CI; /ws remains the streaming variant for the UI.
func (s *Server) registerChatRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "chat",
		Method:      http.MethodPost,
		Path:        "/api/chat",
		Summary:     "Chat with an agent (non-streaming)",
		Description: "Runs one agent turn synchronously and returns the final reply plus a tool-trace summary. For streaming, use the /ws WebSocket. Images are names of the agent's stored reference images (only effective with a vision-capable provider).",
		Tags:        []string{"Chat"},
	}, func(ctx context.Context, input *chatInput) (*chatOutput, error) {
		body := input.Body
		if body.Agent == "" {
			body.Agent = "default"
		}
		if body.Session == "" {
			body.Session = "default"
		}
		if strings.TrimSpace(body.Message) == "" {
			return nil, s.logErr(huma.Error400BadRequest("message is required"))
		}

		// Resolve referenced agent images into base64 data-URLs, same as /ws.
		var images []string
		for _, name := range body.Images {
			if dataURL, err := s.imageDataURL(body.Agent, name); err == nil {
				images = append(images, dataURL)
			}
		}

		var toolsUsed []chatToolCall
		emit := func(ev agent.Event) {
			if ev.Kind == "tool_call" {
				toolsUsed = append(toolsUsed, chatToolCall{Name: ev.ToolName, Args: ev.ToolArgs})
			}
		}
		reply, err := s.engine.Run(ctx, body.Agent, body.Session, body.Message, body.Provider, images, emit)
		if err != nil {
			return nil, s.logErr(huma.Error502BadGateway(err.Error()))
		}

		out := &chatOutput{}
		out.Body.Reply = reply
		out.Body.Agent = body.Agent
		out.Body.Session = body.Session
		if toolsUsed == nil {
			toolsUsed = []chatToolCall{}
		}
		out.Body.ToolsUsed = toolsUsed

		if body.Evolve {
			if err := s.engine.Evolve(ctx, body.Agent, body.Session); err != nil {
				s.log.Error("post-chat evolve failed", "agent", body.Agent, "session", body.Session, "error", err)
			} else {
				out.Body.Evolved = true
			}
		}
		return out, nil
	})
}

// --- Context files ---

func (s *Server) registerContextFileRoutes(api huma.API) {
	tag := []string{"Context Files"}

	huma.Register(api, huma.Operation{
		OperationID: "read-context-file",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/files/{name}",
		Summary:     "Read a context file",
		Description: "Reads one of the agent's context files (AGENTS.md, SOUL.md, IDENTITY.md, USER.md, USER_PREDEFINED.md, CAPABILITIES.md, HEARTBEAT.md).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key  string `path:"key" doc:"Agent key"`
		Name string `path:"name" doc:"Context file name"`
	}) (*fileContentOutput, error) {
		content, err := s.agents.ReadFile(input.Key, input.Name)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		out := &fileContentOutput{}
		out.Body.Name = input.Name
		out.Body.Content = content
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "write-context-file",
		Method:      http.MethodPut,
		Path:        "/api/agents/{key}/files/{name}",
		Summary:     "Write a context file",
		Description: "Creates or replaces one of the agent's context files.",
		Tags:        tag,
	}, func(ctx context.Context, input *writeFileInput) (*statusOutput, error) {
		if err := s.agents.WriteFile(input.Key, input.Name, input.Body.Content); err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "saved"}}, nil
	})
}

// --- Skills ---

func (s *Server) registerSkillRoutes(api huma.API) {
	tag := []string{"Skills"}

	huma.Register(api, huma.Operation{
		OperationID: "list-agent-skills",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/skills",
		Summary:     "List agent skills",
		Description: "Lists the global library skills annotated with this agent's enabled state (Agents → Skills tab).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []skillWithState }, error) {
		ag, err := s.agents.Get(input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		lib, err := s.loadLibrary()
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		enabled := map[string]bool{}
		for _, k := range ag.Config.EnabledSkills {
			enabled[k] = true
		}
		out := make([]skillWithState, 0, len(lib))
		for _, sk := range lib {
			out = append(out, skillWithState{Skill: sk, Enabled: enabled[sk.Key]})
		}
		return &struct{ Body []skillWithState }{Body: out}, nil
	})

	register := func(op huma.Operation, on bool) {
		huma.Register(api, op, func(ctx context.Context, input *struct {
			Key   string `path:"key" doc:"Agent key"`
			Skill string `path:"skill" doc:"Skill key from the global library"`
		}) (*skillStateOutput, error) {
			if err := s.setSkillEnabled(input.Key, input.Skill, on); err != nil {
				return nil, err
			}
			out := &skillStateOutput{}
			out.Body.Agent = input.Key
			out.Body.Skill = input.Skill
			out.Body.Enabled = on
			return out, nil
		})
	}
	register(huma.Operation{
		OperationID: "enable-skill",
		Method:      http.MethodPut,
		Path:        "/api/agents/{key}/skills/{skill}",
		Summary:     "Enable a skill for an agent",
		Description: "Enables a library skill so it is injected into this agent's system prompt.",
		Tags:        tag,
	}, true)
	register(huma.Operation{
		OperationID: "disable-skill",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/skills/{skill}",
		Summary:     "Disable a skill for an agent",
		Description: "Removes a skill from this agent's enabled list.",
		Tags:        tag,
	}, false)

	huma.Register(api, huma.Operation{
		OperationID: "list-skills",
		Method:      http.MethodGet,
		Path:        "/api/skills",
		Summary:     "List library skills",
		Description: "Lists every skill in the global skills library (Skills page). Skills are never inherited; they are enabled per agent.",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []skills.Skill }, error) {
		lib, err := s.loadLibrary()
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if lib == nil {
			lib = []skills.Skill{}
		}
		return &struct{ Body []skills.Skill }{Body: lib}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "upload-skill",
		Method:        http.MethodPost,
		Path:          "/api/skills/upload",
		Summary:       "Upload a skill",
		Description:   "Installs a skill from an uploaded ZIP (multipart field \"file\") into the global skills library. It is not enabled for any agent automatically.",
		Tags:          tag,
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  25 << 20, // 25 MB
	}, func(ctx context.Context, input *struct {
		RawBody multipart.Form
	}) (*struct{ Body *skills.Skill }, error) {
		data, err := firstMultipartFile(&input.RawBody, "file", 25<<20)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		lib, err := s.agents.SkillsLibraryDir()
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		sk, err := skills.InstallZip(lib, data)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body *skills.Skill }{Body: sk}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-skill",
		Method:      http.MethodDelete,
		Path:        "/api/skills/{key}",
		Summary:     "Delete a skill",
		Description: "Removes a skill from the global library (existing per-agent enablements stop resolving).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Skill key"`
	}) (*struct {
		Body struct {
			Deleted string `json:"deleted" example:"code-review"`
		}
	}, error) {
		lib, err := s.agents.SkillsLibraryDir()
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if err := skills.Delete(lib, input.Key); err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		out := &struct {
			Body struct {
				Deleted string `json:"deleted" example:"code-review"`
			}
		}{}
		out.Body.Deleted = input.Key
		return out, nil
	})
}

// --- Agent images (vision) ---

func (s *Server) registerImageRoutes(api huma.API) {
	tag := []string{"Images"}

	huma.Register(api, huma.Operation{
		OperationID: "list-images",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/images",
		Summary:     "List agent images",
		Description: "Lists an agent's stored reference images (used as vision-model attachments).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []agents.Image }, error) {
		imgs, err := s.agents.ListImages(input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body []agents.Image }{Body: imgs}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "upload-image",
		Method:        http.MethodPost,
		Path:          "/api/agents/{key}/images",
		Summary:       "Upload an agent image",
		Description:   "Stores one image (multipart field \"file\", png/jpg/jpeg/gif/webp) for the agent.",
		Tags:          tag,
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  10 << 20, // 10 MB
	}, func(ctx context.Context, input *struct {
		Key     string `path:"key" doc:"Agent key"`
		RawBody multipart.Form
	}) (*struct{ Body *agents.Image }, error) {
		data, filename, err := firstMultipartFileNamed(&input.RawBody, "file", 9<<20)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		img, err := s.agents.SaveImage(input.Key, filename, data)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body *agents.Image }{Body: img}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-image",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/images/{name}",
		Summary:     "Delete an agent image",
		Description: "Removes one of the agent's stored images.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key  string `path:"key" doc:"Agent key"`
		Name string `path:"name" doc:"Image filename"`
	}) (*struct {
		Body struct {
			Deleted string `json:"deleted" example:"diagram.png"`
		}
	}, error) {
		if err := s.agents.DeleteImage(input.Key, input.Name); err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		out := &struct {
			Body struct {
				Deleted string `json:"deleted" example:"diagram.png"`
			}
		}{}
		out.Body.Deleted = input.Name
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "vision-status",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/vision",
		Summary:     "Agent vision status",
		Description: "Reports whether the agent may attach images, given the optional ?provider= override the chat UI passes for the selected provider.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key      string `path:"key" doc:"Agent key"`
		Provider string `query:"provider" doc:"Optional provider name override"`
	}) (*visionOutput, error) {
		ag, err := s.agents.Get(input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		out := &visionOutput{}
		out.Body.Vision = s.engine.EffectiveVision(ag, input.Provider)
		return out, nil
	})
}

// --- Evolve ---

func (s *Server) registerEvolveRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "evolve-agent",
		Method:      http.MethodPost,
		Path:        "/api/evolve",
		Summary:     "Run self-evolution",
		Description: "Extracts durable learnings from a conversation session into the agent's knowledge store.",
		Tags:        []string{"Evolve"},
	}, func(ctx context.Context, input *evolveInput) (*statusOutput, error) {
		agentKey := input.Body.Agent
		if agentKey == "" {
			agentKey = "default"
		}
		session := input.Body.Session
		if session == "" {
			session = "default"
		}
		if err := s.engine.Evolve(ctx, agentKey, session); err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "evolved"}}, nil
	})
}

// --- Knowledge ---

func (s *Server) registerKnowledgeRoutes(api huma.API) {
	tag := []string{"Knowledge"}

	huma.Register(api, huma.Operation{
		OperationID: "list-knowledge",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/knowledge",
		Summary:     "List knowledge entries",
		Description: "Lists the agent's curated long-term knowledge entries (newest first).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []store.KnowledgeEntry }, error) {
		k, err := s.store.Knowledge(ctx, input.Key, 200)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if k == nil {
			k = []store.KnowledgeEntry{}
		}
		return &struct{ Body []store.KnowledgeEntry }{Body: k}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "search-knowledge",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/knowledge/search",
		Summary:     "Search knowledge",
		Description: "Full-text search over the agent's curated knowledge entries (FTS5).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
		Q   string `query:"q" doc:"Search query"`
	}) (*struct{ Body []string }, error) {
		hits, err := s.store.SearchKnowledge(ctx, input.Key, input.Q, 20)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if hits == nil {
			hits = []string{}
		}
		return &struct{ Body []string }{Body: hits}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-knowledge",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/knowledge/{id}",
		Summary:     "Delete a knowledge entry",
		Description: "Removes one curated knowledge entry (scoped to the agent) so wrong/outdated memories can be deleted from the Memory UI.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
		ID  int64  `path:"id" doc:"Knowledge entry ID"`
	}) (*statusOutput, error) {
		if input.ID <= 0 {
			return nil, s.logErr(huma.Error400BadRequest("invalid knowledge id"))
		}
		if err := s.store.DeleteKnowledge(ctx, input.ID, input.Key); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, s.logErr(huma.Error404NotFound("knowledge entry not found"))
			}
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
	})
}

// --- Observations ---

func (s *Server) registerObservationRoutes(api huma.API) {
	tag := []string{"Observations"}

	huma.Register(api, huma.Operation{
		OperationID: "list-observations",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/observations",
		Summary:     "List observations",
		Description: "Lists the agent's timestamped observations (high-churn findings from recurring agents).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []store.Observation }, error) {
		obs, err := s.store.ListObservations(ctx, input.Key, 100)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		return &struct{ Body []store.Observation }{Body: obs}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-observation",
		Method:        http.MethodPost,
		Path:          "/api/agents/{key}/observations",
		Summary:       "Record an observation",
		Description:   "Records a timestamped observation for the agent (subject to retention pruning).",
		Tags:          tag,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, input *struct {
		Key  string `path:"key" doc:"Agent key"`
		Body struct {
			Content string `json:"content" doc:"Observation text"`
		}
	}) (*statusOutput, error) {
		if err := s.store.AddObservation(ctx, input.Key, input.Body.Content); err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "recorded"}}, nil
	})
}

// --- Knowledge-base documents ---

func (s *Server) registerDocRoutes(api huma.API) {
	tag := []string{"Knowledge Docs"}

	huma.Register(api, huma.Operation{
		OperationID: "list-docs",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/docs",
		Summary:     "List knowledge documents",
		Description: "Lists the agent's uploaded reference documents (metadata only, newest first).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []store.KnowledgeDoc }, error) {
		docs, err := s.store.ListKnowledgeDocs(ctx, input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		return &struct{ Body []store.KnowledgeDoc }{Body: docs}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-doc",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/docs/{id}",
		Summary:     "Get a knowledge document",
		Description: "Fetches one document including its full content.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
		ID  int64  `path:"id" doc:"Document ID"`
	}) (*struct{ Body *store.KnowledgeDoc }, error) {
		doc, err := s.store.GetKnowledgeDoc(ctx, input.ID)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &struct{ Body *store.KnowledgeDoc }{Body: doc}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-doc",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/docs/{id}",
		Summary:     "Delete a knowledge document",
		Description: "Removes one of the agent's reference documents.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
		ID  int64  `path:"id" doc:"Document ID"`
	}) (*statusOutput, error) {
		if err := s.store.DeleteKnowledgeDoc(ctx, input.ID); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "search-docs",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/docs/search/query",
		Summary:     "Search knowledge documents",
		Description: "Full-text search over the agent's reference documents (FTS5 over content).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
		Q   string `query:"q" doc:"Search query"`
	}) (*struct{ Body []store.KnowledgeDoc }, error) {
		docs, err := s.store.SearchDocs(ctx, input.Key, input.Q, 20)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if docs == nil {
			docs = []store.KnowledgeDoc{}
		}
		return &struct{ Body []store.KnowledgeDoc }{Body: docs}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-doc",
		Method:        http.MethodPost,
		Path:          "/api/agents/{key}/docs",
		Summary:       "Add a knowledge document",
		Description:   "Accepts a document either as multipart file upload (field \"file\") or as JSON {\"title\": \"...\", \"content\": \"...\"}.",
		Tags:          tag,
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  20 << 20, // 20 MB
	}, func(ctx context.Context, input *struct {
		Key         string `path:"key" doc:"Agent key"`
		ContentType string `header:"Content-Type"`
		RawBody     []byte
	}) (*struct {
		Body struct {
			ID    int64  `json:"id" doc:"New document ID"`
			Title string `json:"title" doc:"Document title"`
		}
	}, error) {
		var title, content string
		if strings.HasPrefix(input.ContentType, "multipart/form-data") {
			t, data, err := parseMultipartFileBody(input.ContentType, input.RawBody, "file", 20<<20)
			if err != nil {
				return nil, s.logErr(huma.Error400BadRequest(err.Error()))
			}
			title = t
			content = string(data)
		} else {
			var body struct {
				Title   string `json:"title"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(input.RawBody, &body); err != nil {
				return nil, s.logErr(huma.Error400BadRequest(err.Error()))
			}
			if strings.TrimSpace(body.Title) == "" || strings.TrimSpace(body.Content) == "" {
				return nil, s.logErr(huma.Error400BadRequest("title and content are required"))
			}
			title = body.Title
			content = body.Content
		}
		id, err := s.store.AddKnowledgeDoc(ctx, input.Key, title, content)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		out := &struct {
			Body struct {
				ID    int64  `json:"id" doc:"New document ID"`
				Title string `json:"title" doc:"Document title"`
			}
		}{}
		out.Body.ID = id
		out.Body.Title = title
		return out, nil
	})
}

// --- Sessions ---

func (s *Server) registerSessionRoutes(api huma.API) {
	tag := []string{"Sessions"}

	huma.Register(api, huma.Operation{
		OperationID: "list-sessions",
		Method:      http.MethodGet,
		Path:        "/api/sessions",
		Summary:     "List conversations",
		Description: "Lists known agent+session conversation pairs, most recent first. Filter with ?agent=.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Agent string `query:"agent" doc:"Optional agent key filter"`
	}) (*struct{ Body []store.Session }, error) {
		sessions, err := s.store.ListSessions(ctx, input.Agent)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		return &struct{ Body []store.Session }{Body: sessions}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "session-messages",
		Method:      http.MethodGet,
		Path:        "/api/sessions/{agent}/{session}/messages",
		Summary:     "Get conversation messages",
		Description: "Returns the stored messages of one conversation (chronological, capped at 200).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Agent   string `path:"agent" doc:"Agent key"`
		Session string `path:"session" doc:"Session name"`
	}) (*struct{ Body []store.Message }, error) {
		msgs, err := s.store.Messages(ctx, input.Agent, input.Session, 200)
		if err != nil {
			return nil, s.logErr(huma.Error500InternalServerError(err.Error()))
		}
		if msgs == nil {
			msgs = []store.Message{}
		}
		return &struct{ Body []store.Message }{Body: msgs}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-session",
		Method:      http.MethodDelete,
		Path:        "/api/sessions/{agent}/{session}",
		Summary:     "Delete a conversation",
		Description: "Removes a whole agent+session conversation history.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Agent   string `path:"agent" doc:"Agent key"`
		Session string `path:"session" doc:"Session name"`
	}) (*deleteSessionOutput, error) {
		if err := s.store.DeleteSession(ctx, input.Agent, input.Session); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		out := &deleteSessionOutput{}
		out.Body.Deleted = input.Session
		return out, nil
	})
}

// --- Capabilities (built-in tools, core tools, extra commands) ---

func (s *Server) registerToolRoutes(api huma.API) {
	tag := []string{"Tools"}

	huma.Register(api, huma.Operation{
		OperationID: "list-tools",
		Method:      http.MethodGet,
		Path:        "/api/tools",
		Summary:     "List built-in tools",
		Description: "Lists the built-in tools available under the global AGENTICGO_TOOL_ALLOWLIST ceiling.",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*listToolsOutput, error) {
		specs := s.tools.Specs()
		out := make([]builtinToolInfo, 0, len(specs))
		for _, sp := range specs {
			out = append(out, builtinToolInfo{Name: sp.Function.Name, Description: sp.Function.Description})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return &listToolsOutput{Body: out}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-core-tools",
		Method:      http.MethodGet,
		Path:        "/api/tools/core",
		Summary:     "List core agent tools",
		Description: "Returns the always-on built-in agent tools (memory + docs recall/save). Wired per run for every agent; cannot be disabled.",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []tools.CoreTool }, error) {
		return &struct{ Body []tools.CoreTool }{Body: tools.CoreTools()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-extra-commands",
		Method:      http.MethodGet,
		Path:        "/api/extra-commands",
		Summary:     "List extra exec commands",
		Description: "Lists the env-declared extra (dangerous) exec commands from AGENTICGO_EXTRA_EXEC_COMMANDS (read-only).",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []string }, error) {
		cmds := s.cfg.ExtraExecCommands
		if cmds == nil {
			cmds = []string{}
		}
		return &struct{ Body []string }{Body: cmds}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-agent-extra-commands",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/extra-commands",
		Summary:     "List agent extra commands",
		Description: "Lists the env-declared extra commands annotated with the agent's enabled state (Agents → Extra Dangerous Exec Commands tab).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []extraCommandWithState }, error) {
		ag, err := s.agents.Get(input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		enabled := map[string]bool{}
		for _, c := range ag.Config.EnabledCommands {
			enabled[c] = true
		}
		out := make([]extraCommandWithState, 0, len(s.cfg.ExtraExecCommands))
		for _, c := range s.cfg.ExtraExecCommands {
			out = append(out, extraCommandWithState{Name: c, Enabled: enabled[c]})
		}
		return &struct{ Body []extraCommandWithState }{Body: out}, nil
	})

	register := func(op huma.Operation, on bool) {
		huma.Register(api, op, func(ctx context.Context, input *struct {
			Key  string `path:"key" doc:"Agent key"`
			Name string `path:"name" doc:"Command name from AGENTICGO_EXTRA_EXEC_COMMANDS"`
		}) (*extraCommandStateOutput, error) {
			if err := s.setExtraCommandEnabled(input.Key, input.Name, on); err != nil {
				return nil, err
			}
			out := &extraCommandStateOutput{}
			out.Body.Agent = input.Key
			out.Body.Command = input.Name
			out.Body.Enabled = on
			return out, nil
		})
	}
	register(huma.Operation{
		OperationID: "enable-extra-command",
		Method:      http.MethodPut,
		Path:        "/api/agents/{key}/extra-commands/{name}",
		Summary:     "Enable an extra exec command",
		Description: "Allows the agent to run this extra (dangerous) command via the exec tool, jailed to its workspace.",
		Tags:        tag,
	}, true)
	register(huma.Operation{
		OperationID: "disable-extra-command",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/extra-commands/{name}",
		Summary:     "Disable an extra exec command",
		Description: "Removes the command from the agent's enabled list.",
		Tags:        tag,
	}, false)

	huma.Register(api, huma.Operation{
		OperationID: "list-agent-builtin-tools",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/builtin-tools",
		Summary:     "List agent built-in tools",
		Description: "Lists the built-in tools annotated with the agent's state (Agents → Built-in Tools tab).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []builtinToolWithState }, error) {
		out, err := s.listAgentBuiltinTools(input.Key)
		if err != nil {
			return nil, err
		}
		return &struct{ Body []builtinToolWithState }{Body: out}, nil
	})

	registerBuiltin := func(op huma.Operation, on bool) {
		huma.Register(api, op, func(ctx context.Context, input *struct {
			Key  string `path:"key" doc:"Agent key"`
			Name string `path:"name" doc:"Built-in tool name"`
		}) (*builtinToolStateOutput, error) {
			if err := s.setBuiltinToolEnabled(input.Key, input.Name, on); err != nil {
				return nil, err
			}
			out := &builtinToolStateOutput{}
			out.Body.Agent = input.Key
			out.Body.Tool = input.Name
			out.Body.Enabled = on
			return out, nil
		})
	}
	registerBuiltin(huma.Operation{
		OperationID: "enable-builtin-tool",
		Method:      http.MethodPut,
		Path:        "/api/agents/{key}/builtin-tools/{name}",
		Summary:     "Enable a built-in tool for an agent",
		Description: "Narrows the agent's built-in tool set to include this tool (never exceeds the global allow-list).",
		Tags:        tag,
	}, true)
	registerBuiltin(huma.Operation{
		OperationID: "disable-builtin-tool",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/builtin-tools/{name}",
		Summary:     "Disable a built-in tool for an agent",
		Description: "Narrows the agent's built-in tool set to exclude this tool.",
		Tags:        tag,
	}, false)

	huma.Register(api, huma.Operation{
		OperationID: "reset-builtin-tools",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/builtin-tools",
		Summary:     "Reset built-in tool narrowing",
		Description: "Clears the per-agent narrowing so the agent inherits the global allow-list again (the default for new agents).",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*resetBuiltinToolsOutput, error) {
		if _, err := s.agents.SetEnabledBuiltinTools(input.Key, nil); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		out := &resetBuiltinToolsOutput{}
		out.Body.Agent = input.Key
		out.Body.BuiltinTools = "inherit"
		return out, nil
	})
}

// --- Providers ---

func (s *Server) registerProviderRoutes(api huma.API) {
	tag := []string{"Providers"}

	huma.Register(api, huma.Operation{
		OperationID: "list-providers",
		Method:      http.MethodGet,
		Path:        "/api/providers",
		Summary:     "List providers",
		Description: "Lists the named OpenAI-compatible provider configs (API keys are redacted by the UI; stored encrypted at rest).",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []providers.Provider }, error) {
		return &struct{ Body []providers.Provider }{Body: s.providers.List()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-provider",
		Method:      http.MethodGet,
		Path:        "/api/providers/{name}",
		Summary:     "Get a provider",
		Description: "Returns one named provider config.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"Provider name"`
	}) (*struct{ Body *providers.Provider }, error) {
		p, err := s.providers.Get(input.Name)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &struct{ Body *providers.Provider }{Body: p}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "upsert-provider",
		Method:      http.MethodPost,
		Path:        "/api/providers",
		Summary:     "Create or update a provider",
		Description: "Creates or replaces a named provider config. API keys are encrypted at rest.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Body providers.Provider
	}) (*struct{ Body providers.Provider }, error) {
		if err := s.providers.Upsert(input.Body); err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body providers.Provider }{Body: input.Body}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-provider",
		Method:      http.MethodDelete,
		Path:        "/api/providers/{name}",
		Summary:     "Delete a provider",
		Description: "Removes a named provider config.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"Provider name"`
	}) (*statusOutput, error) {
		if err := s.providers.Delete(input.Name); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "test-provider-adhoc",
		Method:      http.MethodPost,
		Path:        "/api/providers/test",
		Summary:     "Test a provider config",
		Description: "Tests a posted provider config as-is, so the UI can test the form's current values before saving.",
		Tags:        tag,
	}, func(ctx context.Context, input *testProviderAdhocInput) (*testProviderOutput, error) {
		return s.runProviderTest(ctx, "", input.Body)
	})

	huma.Register(api, huma.Operation{
		OperationID: "test-provider",
		Method:      http.MethodPost,
		Path:        "/api/providers/{name}/test",
		Summary:     "Test a saved provider",
		Description: "Tests the named saved provider, or the posted config when a JSON body is sent.",
		Tags:        tag,
	}, func(ctx context.Context, input *testProviderInput) (*testProviderOutput, error) {
		return s.runProviderTest(ctx, input.Name, input.Body)
	})
}

// runProviderTest tests a provider connection, ad-hoc (posted body) or saved
// (named, empty body).
func (s *Server) runProviderTest(ctx context.Context, name string, adhoc *providers.Provider) (*testProviderOutput, error) {
	models, err := s.providers.TestConnection(ctx, name, adhoc)
	if err != nil {
		return nil, s.logErr(huma.Error502BadGateway(err.Error()))
	}
	if models == nil {
		models = []string{}
	}
	out := &testProviderOutput{}
	out.Body.Status = "ok"
	out.Body.Models = models
	return out, nil
}

// --- MCP servers + tools ---

func (s *Server) registerMCPRoutes(api huma.API) {
	tagServers := []string{"MCP Servers"}
	tagTools := []string{"MCP Tools"}

	huma.Register(api, huma.Operation{
		OperationID: "list-mcp-servers",
		Method:      http.MethodGet,
		Path:        "/api/mcp-servers",
		Summary:     "List MCP servers",
		Description: "Lists the configured MCP servers with their connection status.",
		Tags:        tagServers,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []mcp.ServerStatus }, error) {
		return &struct{ Body []mcp.ServerStatus }{Body: s.mcp.ListServers()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-mcp-server",
		Method:        http.MethodPost,
		Path:          "/api/mcp-servers",
		Summary:       "Add an MCP server",
		Description:   "Registers an MCP server definition (stdio or HTTP). Nothing connects until the Connect action.",
		Tags:          tagServers,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, input *struct {
		Body mcp.ServerConfig
	}) (*struct{ Body *mcp.ServerConfig }, error) {
		created, err := s.mcp.Add(input.Body)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body *mcp.ServerConfig }{Body: created}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-mcp-server",
		Method:      http.MethodPut,
		Path:        "/api/mcp-servers/{id}",
		Summary:     "Update an MCP server",
		Description: "Replaces an MCP server definition.",
		Tags:        tagServers,
	}, func(ctx context.Context, input *struct {
		ID   string `path:"id" doc:"Server ID"`
		Body mcp.ServerConfig
	}) (*struct{ Body *mcp.ServerConfig }, error) {
		updated, err := s.mcp.Update(input.ID, input.Body)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &struct{ Body *mcp.ServerConfig }{Body: updated}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-mcp-server",
		Method:      http.MethodDelete,
		Path:        "/api/mcp-servers/{id}",
		Summary:     "Delete an MCP server",
		Description: "Removes an MCP server definition (disconnecting it first if connected).",
		Tags:        tagServers,
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" doc:"Server ID"`
	}) (*statusOutput, error) {
		if err := s.mcp.Delete(input.ID); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "connect-mcp-server",
		Method:      http.MethodPost,
		Path:        "/api/mcp-servers/{id}/connect",
		Summary:     "Connect an MCP server",
		Description: "Dials the server and discovers its tools. Deliberately manual so heavyweight servers are only spawned when wanted. On failure (502) the body still carries the server status so the UI can show the error next to it.",
		Tags:        tagServers,
		Errors:      []int{http.StatusBadGateway},
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" doc:"Server ID"`
	}) (*connectOutput, error) {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		st, err := s.mcp.Connect(cctx, input.ID)
		if err != nil {
			return nil, s.logErr(&connectError{
				connectErrorBody: connectErrorBody{Error: err.Error(), Server: st},
				status:           http.StatusBadGateway,
			})
		}
		return &connectOutput{Body: st}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "disconnect-mcp-server",
		Method:      http.MethodPost,
		Path:        "/api/mcp-servers/{id}/disconnect",
		Summary:     "Disconnect an MCP server",
		Description: "Closes the connection to the server (the definition stays registered).",
		Tags:        tagServers,
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" doc:"Server ID"`
	}) (*statusOutput, error) {
		if err := s.mcp.Disconnect(input.ID); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "disconnected"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-mcp-tools",
		Method:      http.MethodGet,
		Path:        "/api/mcp-tools",
		Summary:     "List discovered MCP tools",
		Description: "Returns the discovered tool catalog across all connected servers (the tools agents can enable).",
		Tags:        tagTools,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []mcp.ToolInfo }, error) {
		list := s.mcp.Tools()
		if list == nil {
			list = []mcp.ToolInfo{}
		}
		return &struct{ Body []mcp.ToolInfo }{Body: list}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-custom-tools",
		Method:      http.MethodGet,
		Path:        "/api/agents/{key}/custom-tools",
		Summary:     "List agent MCP tools",
		Description: "Lists all discovered MCP tools annotated with the agent's enabled state (Agents → MCP Tools tab).",
		Tags:        tagTools,
	}, func(ctx context.Context, input *struct {
		Key string `path:"key" doc:"Agent key"`
	}) (*struct{ Body []customToolWithState }, error) {
		ag, err := s.agents.Get(input.Key)
		if err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		enabled := map[string]bool{}
		for _, n := range ag.Config.EnabledTools {
			enabled[n] = true
		}
		all := s.mcp.Tools()
		out := make([]customToolWithState, 0, len(all))
		for _, t := range all {
			dn := t.DiscoveredName()
			out = append(out, customToolWithState{ToolInfo: t, Discovered: dn, Enabled: enabled[dn]})
		}
		return &struct{ Body []customToolWithState }{Body: out}, nil
	})

	register := func(op huma.Operation, on bool) {
		huma.Register(api, op, func(ctx context.Context, input *struct {
			Key  string `path:"key" doc:"Agent key"`
			Name string `path:"name" doc:"Namespaced MCP tool name (mcp_<server>_<tool>)"`
		}) (*customToolStateOutput, error) {
			if err := s.setCustomToolEnabled(input.Key, input.Name, on); err != nil {
				return nil, err
			}
			out := &customToolStateOutput{}
			out.Body.Agent = input.Key
			out.Body.Tool = input.Name
			out.Body.Enabled = on
			return out, nil
		})
	}
	register(huma.Operation{
		OperationID: "enable-custom-tool",
		Method:      http.MethodPut,
		Path:        "/api/agents/{key}/custom-tools/{name}",
		Summary:     "Enable an MCP tool for an agent",
		Description: "Allows the agent to call this discovered MCP tool. The tool must currently be discovered (connect its server first).",
		Tags:        tagTools,
	}, true)
	register(huma.Operation{
		OperationID: "disable-custom-tool",
		Method:      http.MethodDelete,
		Path:        "/api/agents/{key}/custom-tools/{name}",
		Summary:     "Disable an MCP tool for an agent",
		Description: "Removes the tool from the agent's enabled list.",
		Tags:        tagTools,
	}, false)
}

// --- Cron jobs (scaffolding) ---

func (s *Server) registerCronRoutes(api huma.API) {
	tag := []string{"Cron"}

	huma.Register(api, huma.Operation{
		OperationID: "list-cron-jobs",
		Method:      http.MethodGet,
		Path:        "/api/cron",
		Summary:     "List cron jobs",
		Description: "Lists the scaffolding cron jobs (API/UI shape only; nothing executes yet).",
		Tags:        tag,
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []scaffold.CronJob }, error) {
		return &struct{ Body []scaffold.CronJob }{Body: s.scaffold.ListCronJobs()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-cron-job",
		Method:        http.MethodPost,
		Path:          "/api/cron",
		Summary:       "Add a cron job",
		Description:   "Registers a cron job definition (scaffolding; not persisted or executed yet).",
		Tags:          tag,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, input *struct {
		Body scaffold.CronJob
	}) (*struct{ Body *scaffold.CronJob }, error) {
		created, err := s.scaffold.AddCronJob(input.Body)
		if err != nil {
			return nil, s.logErr(huma.Error400BadRequest(err.Error()))
		}
		return &struct{ Body *scaffold.CronJob }{Body: created}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-cron-job",
		Method:      http.MethodDelete,
		Path:        "/api/cron/{id}",
		Summary:     "Delete a cron job",
		Description: "Removes a scaffolding cron job.",
		Tags:        tag,
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" doc:"Cron job ID"`
	}) (*statusOutput, error) {
		if err := s.scaffold.DeleteCronJob(input.ID); err != nil {
			return nil, s.logErr(huma.Error404NotFound(err.Error()))
		}
		return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
	})
}
