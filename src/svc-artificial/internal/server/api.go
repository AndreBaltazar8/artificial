package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
)

// Candidate is a recruitment candidate (in-memory only, never persisted).
type Candidate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Persona     string `json:"persona"`
	Description string `json:"description"` // original request
}

var (
	candidatesMu sync.Mutex
	candidates   = map[string]Candidate{} // id → candidate
)

func (s *Server) registerAPI() {
	s.Mux.HandleFunc("GET /api/employees", s.apiListEmployees)
	s.Mux.HandleFunc("POST /api/employees", s.apiCreateEmployee)
	s.Mux.HandleFunc("PUT /api/employees/bulk-config", s.apiBulkUpdateEmployeeConfig)
	s.Mux.HandleFunc("GET /api/employees/{id}", s.apiGetEmployee)
	s.Mux.HandleFunc("PUT /api/employees/{id}", s.apiUpdateEmployee)
	s.Mux.HandleFunc("GET /api/employees/suggest-name", s.apiSuggestName)
	s.Mux.HandleFunc("POST /api/employees/generate-persona", s.apiGeneratePersona)

	s.Mux.HandleFunc("GET /api/projects", s.apiListProjects)
	s.Mux.HandleFunc("POST /api/projects", s.apiCreateProject)
	s.Mux.HandleFunc("DELETE /api/projects/{id}", s.apiDeleteProject)
	s.Mux.HandleFunc("GET /api/projects/{id}/agents", s.apiListProjectAgents)
	s.Mux.HandleFunc("POST /api/projects/{id}/agents", s.apiAddProjectAgents)
	s.Mux.HandleFunc("PUT /api/projects/{id}/agents", s.apiSetProjectAgents)
	s.Mux.HandleFunc("DELETE /api/projects/{id}/agents/{employeeID}", s.apiRemoveProjectAgent)
	s.Mux.HandleFunc("POST /api/projects/{id}/spawn-assigned", s.apiSpawnProjectAgents)

	s.Mux.HandleFunc("GET /api/channels", s.apiListChannels)
	s.Mux.HandleFunc("GET /api/dm-channels", s.apiListDMChannels)
	s.Mux.HandleFunc("GET /api/messages", s.apiGetMessages)
	s.Mux.HandleFunc("GET /api/channels/{name}/members", s.apiGetChannelMembers)

	s.Mux.HandleFunc("GET /api/workers", s.apiListWorkers)
	s.Mux.HandleFunc("POST /api/workers", s.apiCreateWorker)
	s.Mux.HandleFunc("POST /api/workers/spawn", s.apiSpawnWorker)
	s.Mux.HandleFunc("POST /api/workers/respawn-all", s.apiRespawnAll)
	s.Mux.HandleFunc("POST /api/workers/kill-all", s.apiKillAll)
	s.Mux.HandleFunc("PUT /api/workers/{id}/status", s.apiUpdateWorkerStatus)
	s.Mux.HandleFunc("PUT /api/workers/{id}/session", s.apiSetWorkerSession)

	s.Mux.HandleFunc("GET /api/tasks", s.apiListTasks)
	s.Mux.HandleFunc("POST /api/tasks", s.apiCreateTask)
	s.Mux.HandleFunc("GET /api/tasks/{id}", s.apiGetTask)
	s.Mux.HandleFunc("PUT /api/tasks/{id}", s.apiUpdateTask)
	s.Mux.HandleFunc("DELETE /api/tasks/{id}", s.apiDeleteTask)

	s.Mux.HandleFunc("GET /api/settings", s.apiGetSettings)
	s.Mux.HandleFunc("PUT /api/settings", s.apiUpdateSettings)
	s.Mux.HandleFunc("POST /api/recruit", s.apiRecruit)
	s.Mux.HandleFunc("POST /api/recruit/accept", s.apiRecruitAccept)
	s.Mux.HandleFunc("GET /api/reviews", s.apiListReviews)
	s.Mux.HandleFunc("POST /api/reviews/{id}/respond", s.apiRespondToReview)
	s.Mux.HandleFunc("GET /api/files", s.apiServeFile)
	s.Mux.HandleFunc("GET /api/unread/{employeeID}", s.apiGetUnread)
	s.Mux.HandleFunc("GET /api/workers/{id}/logs", s.apiStreamWorkerLogs)
	s.Mux.HandleFunc("POST /api/workers/{id}/kill", s.apiKillWorker)
	s.Mux.HandleFunc("PUT /api/workers/{id}/transcript", s.apiSetWorkerTranscript)
	s.Mux.HandleFunc("GET /api/workers/{id}/transcript", s.apiStreamWorkerTranscript)
	s.Mux.HandleFunc("GET /api/workers/{id}/tty", s.apiStreamWorkerTTY)

	s.Mux.HandleFunc("GET /api/plugins", s.apiListPlugins)
	s.Mux.HandleFunc("POST /api/plugins", s.apiCreatePlugin)
	s.Mux.HandleFunc("PATCH /api/plugins/{id}", s.apiUpdatePlugin)
	s.Mux.HandleFunc("POST /api/plugins/{id}/reload", s.apiReloadPlugin)
	s.Mux.HandleFunc("DELETE /api/plugins/{id}", s.apiDeletePlugin)

	s.registerRunnerAPI()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func pathID(r *http.Request, name string) int64 {
	v := r.PathValue(name)
	id, _ := strconv.ParseInt(v, 10, 64)
	return id
}

// ── Employees ───────────────────────────────────────────────────────────

func (s *Server) apiListEmployees(w http.ResponseWriter, r *http.Request) {
	employees, err := s.DB.ListEmployees()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, employees)
}

func (s *Server) apiCreateEmployee(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Nickname string `json:"nickname"`
		Role     string `json:"role"`
		Persona  string `json:"persona"`
		Email    string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.Nickname == "" {
		writeErr(w, 400, "nickname required")
		return
	}
	emp, err := s.DB.CreateEmployee(input.Nickname, input.Role, input.Persona, input.Email)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, 409, "nickname already exists")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	// Auto-join general channel
	s.DB.JoinChannel("general", emp.ID)
	w.WriteHeader(201)
	writeJSON(w, emp)
}

func (s *Server) apiUpdateEmployee(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == 0 {
		writeErr(w, 400, "invalid id")
		return
	}
	var input struct {
		Persona     *string `json:"persona"`
		Email       *string `json:"email"`
		Employed    *int    `json:"employed"`
		Harness     *string `json:"harness"`
		Model       *string `json:"model"`
		ACPURL      *string `json:"acp_url"`
		ACPProvider *string `json:"acp_provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err := s.DB.UpdateEmployee(id, input.Persona, input.Email); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if input.Employed != nil {
		if err := s.DB.UpdateEmployeeEmployed(id, *input.Employed); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if input.Harness != nil || input.Model != nil || input.ACPURL != nil || input.ACPProvider != nil {
		if err := s.DB.UpdateEmployeeHarness(id, input.Harness, input.Model, input.ACPURL, input.ACPProvider); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	emp, _ := s.DB.GetEmployee(id)
	writeJSON(w, emp)
}

func (s *Server) apiBulkUpdateEmployeeConfig(w http.ResponseWriter, r *http.Request) {
	var input protocol.BulkEmployeeConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if len(input.EmployeeIDs) == 0 {
		writeErr(w, 400, "employee_ids required")
		return
	}
	if input.Harness == nil && input.Model == nil && input.ACPURL == nil && input.ACPProvider == nil {
		writeErr(w, 400, "harness or model required")
		return
	}
	if input.Harness != nil {
		h := strings.TrimSpace(*input.Harness)
		if h != "claude" && h != "codex" && h != "acp" {
			writeErr(w, 400, "invalid harness")
			return
		}
		input.Harness = &h
	}
	if input.Model != nil {
		m := strings.TrimSpace(*input.Model)
		if m == "" {
			writeErr(w, 400, "model cannot be blank")
			return
		}
		input.Model = &m
	}
	if input.ACPProvider != nil {
		p := strings.TrimSpace(*input.ACPProvider)
		if p != "" && p != "opencode" && p != "cursor" {
			writeErr(w, 400, "invalid acp_provider")
			return
		}
		input.ACPProvider = &p
	}
	if input.ACPURL != nil {
		u := strings.TrimSpace(*input.ACPURL)
		input.ACPURL = &u
	}

	seen := map[int64]bool{}
	resp := protocol.BulkEmployeeConfigResponse{}
	for _, id := range input.EmployeeIDs {
		result := protocol.BulkEmployeeConfigResult{EmployeeID: id}
		if id == 0 {
			result.Error = "invalid employee id"
			resp.Results = append(resp.Results, result)
			resp.FailureCount++
			continue
		}
		if seen[id] {
			result.Error = "duplicate employee id"
			resp.Results = append(resp.Results, result)
			resp.FailureCount++
			continue
		}
		seen[id] = true

		emp, err := s.DB.GetEmployee(id)
		if err != nil {
			result.Error = "employee not found"
			resp.Results = append(resp.Results, result)
			resp.FailureCount++
			continue
		}
		result.Nickname = emp.Nickname
		if emp.Role != "worker" {
			result.Error = "only worker agents can be bulk-edited"
			resp.Results = append(resp.Results, result)
			resp.FailureCount++
			continue
		}

		if err := s.DB.UpdateEmployeeHarness(id, input.Harness, input.Model, input.ACPURL, input.ACPProvider); err != nil {
			result.Error = err.Error()
			resp.Results = append(resp.Results, result)
			resp.FailureCount++
			continue
		}
		updated, err := s.DB.GetEmployee(id)
		if err != nil {
			result.Error = err.Error()
			resp.Results = append(resp.Results, result)
			resp.FailureCount++
			continue
		}
		result.OK = true
		resp.Results = append(resp.Results, result)
		resp.Updated = append(resp.Updated, updated)
		resp.SuccessCount++
	}
	if resp.Results == nil {
		resp.Results = []protocol.BulkEmployeeConfigResult{}
	}
	if resp.Updated == nil {
		resp.Updated = []protocol.Employee{}
	}
	writeJSON(w, resp)
}

func (s *Server) apiSuggestName(w http.ResponseWriter, r *http.Request) {
	employees, _ := s.DB.ListEmployees()
	used := make(map[string]bool)
	for _, e := range employees {
		used[e.Nickname] = true
	}
	// Collect available names
	var available []string
	for _, name := range names {
		if !used[name] {
			available = append(available, name)
		}
	}
	if len(available) == 0 {
		writeJSON(w, map[string]string{"name": fmt.Sprintf("agent-%d", time.Now().Unix())})
		return
	}
	// Pick a random one
	writeJSON(w, map[string]string{"name": available[time.Now().UnixNano()%int64(len(available))]})
}

func (s *Server) apiGeneratePersona(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Description string `json:"description"`
		Name        string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.Description == "" {
		writeErr(w, 400, "description required")
		return
	}

	prompt := fmt.Sprintf(`Generate a persona for an AI coding agent named "%s" based on this description: %s

The persona should be 2 short paragraphs. First paragraph: who they are, their strengths, working style. Second paragraph: how they approach work, what drives them. Start with "You are %s, ..."`, input.Name, input.Description, input.Name)

	schema := `{"type":"object","properties":{"persona":{"type":"string","description":"The full persona text"}},"required":["persona"]}`
	cmd := exec.Command("claude", "-p", prompt, "--model", "sonnet", "--output-format", "json", "--json-schema", schema)
	output, err := cmd.Output()
	if err != nil {
		writeErr(w, 500, fmt.Sprintf("claude failed: %v", err))
		return
	}

	var parsed struct {
		StructuredOutput struct {
			Persona string `json:"persona"`
		} `json:"structured_output"`
		Result string `json:"result"`
	}
	json.Unmarshal(output, &parsed)
	persona := parsed.StructuredOutput.Persona
	if persona == "" {
		persona = parsed.Result
	}
	if persona == "" {
		persona = strings.TrimSpace(string(output))
	}

	writeJSON(w, map[string]string{"persona": persona})
}

func (s *Server) apiGetEmployee(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == 0 {
		writeErr(w, 400, "invalid id")
		return
	}
	emp, err := s.DB.GetEmployee(id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	channels, _ := s.DB.GetEmployeeChannels(emp.ID)

	returning := false
	previousSessionID := ""
	if prev, ok := s.DB.GetLatestWorkerForEmployee(emp.ID); ok {
		returning = true
		previousSessionID = prev.SessionID
	}

	knowledgePath, _ := s.DB.GetSetting("company_knowledge_path")

	writeJSON(w, map[string]any{
		"employee":               emp,
		"channels":               channels,
		"returning":              returning,
		"previous_session_id":    previousSessionID,
		"company_knowledge_path": knowledgePath,
	})
}

// ── Projects ────────────────────────────────────────────────────────────

func (s *Server) apiListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.DB.ListProjects()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, projects)
}

func (s *Server) apiCreateProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		GitRemote string `json:"git_remote"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.Name == "" {
		writeErr(w, 400, "name required")
		return
	}
	proj, err := s.DB.CreateProject(input.Name, input.Path, input.GitRemote)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(201)
	writeJSON(w, proj)
}

func (s *Server) apiDeleteProject(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := s.DB.DeleteProject(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) apiListProjectAgents(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if _, err := s.DB.GetProject(id); err != nil {
		writeErr(w, 404, "project not found")
		return
	}
	agents, err := s.DB.ListProjectEmployees(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if agents == nil {
		agents = []protocol.Employee{}
	}
	writeJSON(w, agents)
}

func (s *Server) apiAddProjectAgents(w http.ResponseWriter, r *http.Request) {
	projectID := pathID(r, "id")
	if _, err := s.DB.GetProject(projectID); err != nil {
		writeErr(w, 404, "project not found")
		return
	}
	var input struct {
		EmployeeIDs []int64 `json:"employee_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	employeeIDs, status, msg := s.validateProjectAgentIDs(input.EmployeeIDs)
	if msg != "" {
		writeErr(w, status, msg)
		return
	}
	if len(employeeIDs) == 0 {
		writeErr(w, 400, "employee_ids required")
		return
	}
	if err := s.DB.AssignEmployeesToProject(projectID, employeeIDs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.writeProjectAgents(w, projectID)
}

func (s *Server) apiSetProjectAgents(w http.ResponseWriter, r *http.Request) {
	projectID := pathID(r, "id")
	if _, err := s.DB.GetProject(projectID); err != nil {
		writeErr(w, 404, "project not found")
		return
	}
	var input struct {
		EmployeeIDs []int64 `json:"employee_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	employeeIDs, status, msg := s.validateProjectAgentIDs(input.EmployeeIDs)
	if msg != "" {
		writeErr(w, status, msg)
		return
	}
	if err := s.DB.SetProjectEmployees(projectID, employeeIDs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.writeProjectAgents(w, projectID)
}

func (s *Server) apiRemoveProjectAgent(w http.ResponseWriter, r *http.Request) {
	projectID := pathID(r, "id")
	employeeID := pathID(r, "employeeID")
	if _, err := s.DB.GetProject(projectID); err != nil {
		writeErr(w, 404, "project not found")
		return
	}
	if err := s.DB.RemoveEmployeeFromProject(projectID, employeeID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) apiSpawnProjectAgents(w http.ResponseWriter, r *http.Request) {
	projectID := pathID(r, "id")
	project, err := s.DB.GetProject(projectID)
	if err != nil {
		writeErr(w, 404, "project not found")
		return
	}
	agents, err := s.DB.ListProjectEmployees(projectID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	type agentSpawnResult struct {
		EmployeeID int64  `json:"employee_id"`
		Employee   string `json:"employee"`
		Status     string `json:"status"`
		Message    string `json:"message,omitempty"`
		Error      string `json:"error,omitempty"`
	}

	results := make([]agentSpawnResult, 0, len(agents))
	spawned := make([]string, 0, len(agents))
	successCount := 0
	skippedCount := 0
	failureCount := 0
	for _, agent := range agents {
		if agent.Role != "worker" {
			skippedCount++
			results = append(results, agentSpawnResult{
				EmployeeID: agent.ID,
				Employee:   agent.Nickname,
				Status:     "skipped",
				Message:    "not a worker agent",
			})
			continue
		}
		if existing := s.getActiveWorker(agent.ID); existing != nil {
			skippedCount++
			results = append(results, agentSpawnResult{
				EmployeeID: agent.ID,
				Employee:   agent.Nickname,
				Status:     "skipped",
				Message:    fmt.Sprintf("already running (PID %d)", existing.PID),
			})
			continue
		}
		spawnedResult, _, err := s.spawnWorkerForEmployee(agent.ID)
		if err != nil {
			failureCount++
			results = append(results, agentSpawnResult{
				EmployeeID: agent.ID,
				Employee:   agent.Nickname,
				Status:     "failed",
				Error:      err.Error(),
			})
			continue
		}
		successCount++
		spawned = append(spawned, agent.Nickname)
		results = append(results, agentSpawnResult{
			EmployeeID: agent.ID,
			Employee:   agent.Nickname,
			Status:     "spawned",
			Message:    spawnedResult.Message,
		})
	}

	writeJSON(w, map[string]any{
		"project":       project,
		"results":       results,
		"spawned":       spawned,
		"success_count": successCount,
		"skipped_count": skippedCount,
		"failure_count": failureCount,
		"message":       fmt.Sprintf("Spawned %d, skipped %d, failed %d assigned agent(s)", successCount, skippedCount, failureCount),
	})
}

func (s *Server) writeProjectAgents(w http.ResponseWriter, projectID int64) {
	project, err := s.DB.GetProject(projectID)
	if err != nil {
		writeErr(w, 404, "project not found")
		return
	}
	agents, err := s.DB.ListProjectEmployees(projectID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if agents == nil {
		agents = []protocol.Employee{}
	}
	writeJSON(w, map[string]any{
		"project": project,
		"agents":  agents,
	})
}

func (s *Server) validateProjectAgentIDs(ids []int64) ([]int64, int, string) {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, 400, "invalid employee_id"
		}
		if seen[id] {
			continue
		}
		emp, err := s.DB.GetEmployee(id)
		if err != nil {
			return nil, 404, fmt.Sprintf("employee %d not found", id)
		}
		if emp.Role != "worker" {
			return nil, 400, fmt.Sprintf("%s is not a worker agent", emp.Nickname)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, 0, ""
}

// ── Channels ────────────────────────────────────────────────────────────

func (s *Server) apiListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.DB.ListChannels()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, channels)
}

func (s *Server) apiGetChannelMembers(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	memberIDs, err := s.DB.GetChannelMembers(name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var members []map[string]any
	for _, id := range memberIDs {
		if emp, err := s.DB.GetEmployee(id); err == nil {
			members = append(members, map[string]any{
				"id":       emp.ID,
				"nickname": emp.Nickname,
				"role":     emp.Role,
			})
		}
	}
	if members == nil {
		members = []map[string]any{}
	}
	writeJSON(w, members)
}

func (s *Server) apiListDMChannels(w http.ResponseWriter, r *http.Request) {
	dms, err := s.DB.GetDMChannels()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if dms == nil {
		dms = []map[string]string{}
	}
	writeJSON(w, dms)
}

// ── Messages ────────────────────────────────────────────────────────────

func (s *Server) apiGetMessages(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "general"
	}

	limitStr := r.URL.Query().Get("limit")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	beforeStr := r.URL.Query().Get("before")
	before, _ := strconv.ParseInt(beforeStr, 10, 64)

	sinceStr := r.URL.Query().Get("since")
	since, _ := strconv.ParseInt(sinceStr, 10, 64)

	var msgs []protocol.Message
	var err error

	// Fetch limit+1 to determine has_more
	fetchLimit := limit + 1

	if before > 0 {
		msgs, err = s.DB.GetMessagesBefore(channel, before, fetchLimit)
	} else if since > 0 {
		msgs, err = s.DB.GetMessagesSince(channel, since, fetchLimit)
	} else {
		msgs, err = s.DB.GetMessages(channel, fetchLimit)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if msgs == nil {
		msgs = []protocol.Message{}
	}

	hasMore := len(msgs) > limit
	if hasMore {
		if since > 0 {
			// Forward pagination: trim newest (keep oldest N)
			msgs = msgs[:limit]
		} else {
			// Default or backward pagination: trim oldest (keep newest N)
			msgs = msgs[len(msgs)-limit:]
		}
	}

	writeJSON(w, map[string]any{
		"messages": msgs,
		"has_more": hasMore,
	})
}

// ── Workers ─────────────────────────────────────────────────────────────

func (s *Server) apiListWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.DB.ListWorkers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Reconcile: mark workers whose PIDs are no longer running as offline
	for i := range workers {
		if workers[i].Status != "offline" && (workers[i].PID <= 0 || !isWorkerProcess(workers[i].PID)) {
			s.DB.UpdateWorkerStatus(workers[i].ID, "offline")
			workers[i].Status = "offline"
		}
	}
	writeJSON(w, workers)
}

func (s *Server) apiCreateWorker(w http.ResponseWriter, r *http.Request) {
	var input struct {
		EmployeeID int64 `json:"employee_id"`
		PID        int   `json:"pid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.EmployeeID == 0 {
		writeErr(w, 400, "employee_id required")
		return
	}
	worker, err := s.DB.CreateWorker(input.EmployeeID, input.PID, "")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(201)
	writeJSON(w, worker)
}

func (s *Server) apiSpawnWorker(w http.ResponseWriter, r *http.Request) {
	var input struct {
		EmployeeID int64 `json:"employee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.EmployeeID == 0 {
		writeErr(w, 400, "employee_id required")
		return
	}

	result, status, err := s.spawnWorkerForEmployee(input.EmployeeID)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	w.WriteHeader(status)
	writeJSON(w, result)
}

type spawnWorkerResult struct {
	WorkerID int64  `json:"worker_id"`
	PID      int    `json:"pid"`
	Employee string `json:"employee"`
	Message  string `json:"message"`
}

func (s *Server) spawnWorkerForEmployee(employeeID int64) (spawnWorkerResult, int, error) {
	emp, err := s.DB.GetEmployee(employeeID)
	if err != nil {
		return spawnWorkerResult{}, http.StatusNotFound, fmt.Errorf("employee not found")
	}
	if emp.Role == "commander" {
		return spawnWorkerResult{}, http.StatusBadRequest, fmt.Errorf("cannot spawn a worker for the commander")
	}
	if existing := s.getActiveWorker(employeeID); existing != nil {
		return spawnWorkerResult{}, http.StatusConflict, fmt.Errorf("worker already running for %s (PID %d)", emp.Nickname, existing.PID)
	}

	workerBin := s.WorkerBin
	if workerBin == "" {
		workerBin = findWorkerBin()
	}
	if workerBin == "" {
		return spawnWorkerResult{}, http.StatusInternalServerError, fmt.Errorf("cmd-worker binary not found; set --worker-bin flag")
	}

	serverAddr := fmt.Sprintf("localhost:%d", s.Port)
	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".config", "artificial", "logs")
	os.MkdirAll(logsDir, 0755)
	logPath := filepath.Join(logsDir, fmt.Sprintf("worker-%s-%d.log", emp.Nickname, time.Now().Unix()))

	preWorker, err := s.DB.CreateWorker(employeeID, 0, logPath)
	if err != nil {
		return spawnWorkerResult{}, http.StatusInternalServerError, err
	}

	cmd := exec.Command(workerBin,
		"--server", serverAddr,
		"--employee-id", strconv.FormatInt(employeeID, 10),
		"--worker-id", strconv.FormatInt(preWorker.ID, 10),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil

	logFile, logErr := os.Create(logPath)
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		s.DB.UpdateWorkerStatus(preWorker.ID, "offline")
		return spawnWorkerResult{}, http.StatusInternalServerError, fmt.Errorf("spawn failed: %v", err)
	}

	if logFile != nil {
		logFile.Close()
	}

	pid := cmd.Process.Pid
	cmd.Process.Release()

	s.DB.UpdateWorkerStatus(preWorker.ID, "online")
	s.DB.UpdateWorkerPID(preWorker.ID, pid)
	s.Hub.BroadcastWorkerStatus(emp.Nickname, "online")

	return spawnWorkerResult{
		WorkerID: preWorker.ID,
		PID:      pid,
		Employee: emp.Nickname,
		Message:  fmt.Sprintf("spawned cmd-worker for %s (PID %d)", emp.Nickname, pid),
	}, http.StatusCreated, nil
}

func (s *Server) apiRespawnAll(w http.ResponseWriter, r *http.Request) {
	employees, _ := s.DB.ListEmployees()
	var toSpawn []protocol.Employee
	for _, emp := range employees {
		if emp.Role == "commander" {
			continue
		}
		if emp.Employed != 1 {
			continue
		}
		if existing := s.getActiveWorker(emp.ID); existing != nil {
			continue
		}
		toSpawn = append(toSpawn, emp)
	}

	// Spawn in background with 10s delay between each
	go func() {
		for i, emp := range toSpawn {
			if i > 0 {
				time.Sleep(10 * time.Second)
			}
			body, _ := json.Marshal(map[string]any{"employee_id": emp.ID})
			req, _ := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d/api/workers/spawn", s.Port), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := &responseCapture{headers: http.Header{}}
			s.apiSpawnWorker(rec, req)
			slog.Info("respawn-all: spawned", "nick", emp.Nickname, "index", i+1, "total", len(toSpawn))
		}
	}()

	names := make([]string, len(toSpawn))
	for i, e := range toSpawn {
		names[i] = e.Nickname
	}
	writeJSON(w, map[string]any{
		"spawning": names,
		"count":    len(toSpawn),
		"message":  fmt.Sprintf("Spawning %d worker(s) with 10s delay between each", len(toSpawn)),
	})
}

func (s *Server) apiKillAll(w http.ResponseWriter, r *http.Request) {
	workers, _ := s.DB.ListWorkers()
	var killed []string
	for _, wk := range workers {
		if wk.Status == "offline" {
			continue
		}
		if wk.PID > 0 {
			if proc, err := os.FindProcess(wk.PID); err == nil {
				proc.Signal(syscall.SIGTERM)
			}
		}
		s.DB.UpdateWorkerStatus(wk.ID, "offline")
		if emp, err := s.DB.GetEmployee(wk.EmployeeID); err == nil {
			killed = append(killed, emp.Nickname)
			s.Hub.BroadcastWorkerStatus(emp.Nickname, "offline")
		}
	}
	writeJSON(w, map[string]any{
		"killed": killed,
		"count":  len(killed),
	})
}

// responseCapture is a minimal ResponseWriter for internal API calls.
type responseCapture struct {
	headers http.Header
	status  int
	body    bytes.Buffer
}

func (r *responseCapture) Header() http.Header         { return r.headers }
func (r *responseCapture) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *responseCapture) WriteHeader(s int)           { r.status = s }

func (s *Server) apiUpdateWorkerStatus(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var input struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err := s.DB.UpdateWorkerStatus(id, input.Status); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// getActiveWorker returns the latest non-offline worker for an employee if its
// PID is still a running cmd-worker process. Marks stale workers as offline.
func (s *Server) getActiveWorker(employeeID int64) *protocol.Worker {
	workers, err := s.DB.ListWorkers()
	if err != nil {
		return nil
	}
	for i := range workers {
		w := &workers[i]
		if w.EmployeeID != employeeID || w.Status == "offline" {
			continue
		}
		if w.PID <= 0 || !isWorkerProcess(w.PID) {
			// Stale — mark offline
			s.DB.UpdateWorkerStatus(w.ID, "offline")
			continue
		}
		return w
	}
	return nil
}

// isWorkerProcess checks if a PID is a running cmd-worker process.
// Uses `ps` so it works on both Linux and macOS (no /proc on mac).
func isWorkerProcess(pid int) bool {
	// Check if process exists
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without actually sending a signal
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	// Verify it's actually cmd-worker via `ps -p <pid> -o command=`
	// The `command=` form suppresses the header and prints the full command.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "cmd-worker")
}

// ── Tasks ───────────────────────────────────────────────────────────────

func (s *Server) apiListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	assignee := r.URL.Query().Get("assignee")
	projStr := r.URL.Query().Get("project_id")
	projID, _ := strconv.ParseInt(projStr, 10, 64)

	result, err := s.DB.ListTasks(status, assignee, projID, 0)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tasks := result.Tasks
	if tasks == nil {
		writeJSON(w, []struct{}{})
		return
	}
	writeJSON(w, tasks)
}

func (s *Server) apiCreateTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Assignee    string `json:"assignee"`
		ProjectID   int64  `json:"project_id"`
		CreatedBy   string `json:"created_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.Title == "" {
		writeErr(w, 400, "title required")
		return
	}
	if input.CreatedBy == "" {
		input.CreatedBy = "commander"
	}
	task, err := s.DB.CreateTask(input.Title, input.Description, input.Assignee, input.ProjectID, input.CreatedBy)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(201)
	writeJSON(w, task)
}

func (s *Server) apiGetTask(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	task, err := s.DB.GetTask(id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	result := map[string]any{"task": task}
	if task.ProjectID > 0 {
		if proj, err := s.DB.GetProject(task.ProjectID); err == nil {
			result["project"] = proj
		}
	}
	writeJSON(w, result)
}

func (s *Server) apiUpdateTask(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var input struct {
		Status    *string `json:"status"`
		Assignee  *string `json:"assignee"`
		ProjectID *int64  `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.ProjectID != nil && *input.ProjectID > 0 {
		if _, err := s.DB.GetProject(*input.ProjectID); err != nil {
			writeErr(w, 400, "project_id does not match an existing project")
			return
		}
	}
	prev, _ := s.DB.GetTask(id)
	task, err := s.DB.UpdateTask(id, input.Status, input.Assignee, input.ProjectID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Auto-spawn a runner the moment a task transitions into
	// in_progress. If a runner already exists for this task (manager
	// pre-spawned, dashboard race) spawnRunnerForTask returns an
	// error which we log but don't surface — the task update itself
	// succeeded, the runner spawn is best-effort.
	if input.Status != nil && *input.Status == "in_progress" && prev.Status != "in_progress" {
		go func(taskID int64, parent string) {
			if _, err := s.spawnRunnerForTask(taskID, parent); err != nil {
				slog.Info("auto-spawn runner skipped or failed", "task_id", taskID, "err", err)
			}
		}(id, task.Assignee)
	}
	writeJSON(w, task)
}

func (s *Server) apiDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := s.DB.DeleteTask(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// ── Unread ──────────────────────────────────────────────────────────────

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.DB.GetAllSettings()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, settings)
}

func (s *Server) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var input map[string]string
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	for k, v := range input {
		s.DB.SetSetting(k, v)
	}
	w.WriteHeader(204)
}

// apiRecruit generates 3 candidate employees from a description.
func (s *Server) apiRecruit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Description string `json:"description"`
		Count       int    `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if input.Description == "" {
		writeErr(w, 400, "description required")
		return
	}
	if input.Count <= 0 || input.Count > 5 {
		input.Count = 3
	}

	// Get available names
	employees, _ := s.DB.ListEmployees()
	used := make(map[string]bool)
	for _, e := range employees {
		used[e.Nickname] = true
	}
	var available []string
	for _, name := range names {
		if !used[name] {
			available = append(available, name)
		}
	}

	// Pick random names
	picked := make([]string, 0, input.Count)
	for i := 0; i < input.Count && len(available) > 0; i++ {
		idx := time.Now().UnixNano() % int64(len(available))
		picked = append(picked, available[idx])
		available = append(available[:idx], available[idx+1:]...)
	}

	// Generate all personas in a single Claude call
	namesJSON, _ := json.Marshal(picked)
	prompt := fmt.Sprintf(`Generate %d distinct personas for AI coding agents with these names: %s

They should all match this job description: %s

Each persona should be 2 short paragraphs with slightly different personalities and traits. Start each with "You are <name>, ..."`, len(picked), string(namesJSON), input.Description)

	itemSchema := `{"type":"object","properties":{"name":{"type":"string"},"persona":{"type":"string"}},"required":["name","persona"]}`
	schema := fmt.Sprintf(`{"type":"object","properties":{"candidates":{"type":"array","items":%s}},"required":["candidates"]}`, itemSchema)

	cmd := exec.Command("claude", "-p", prompt, "--model", "sonnet", "--output-format", "json", "--json-schema", schema)
	out, _ := cmd.Output()

	var parsed struct {
		StructuredOutput struct {
			Candidates []struct {
				Name    string `json:"name"`
				Persona string `json:"persona"`
			} `json:"candidates"`
		} `json:"structured_output"`
	}
	json.Unmarshal(out, &parsed)

	// Build candidates, matching generated personas to picked names
	cands := make([]Candidate, len(picked))
	for i, name := range picked {
		b := make([]byte, 4)
		rand.Read(b)
		id := hex.EncodeToString(b)
		persona := fmt.Sprintf("You are %s, a skilled worker specializing in: %s", name, input.Description)
		// Find matching persona from Claude's output
		for _, c := range parsed.StructuredOutput.Candidates {
			if strings.EqualFold(c.Name, name) && c.Persona != "" {
				persona = c.Persona
				break
			}
		}
		// Fallback: use by index if name matching failed
		if persona == fmt.Sprintf("You are %s, a skilled worker specializing in: %s", name, input.Description) {
			if i < len(parsed.StructuredOutput.Candidates) && parsed.StructuredOutput.Candidates[i].Persona != "" {
				persona = parsed.StructuredOutput.Candidates[i].Persona
			}
		}
		cands[i] = Candidate{
			ID:          id,
			Name:        name,
			Persona:     persona,
			Description: input.Description,
		}
	}

	// Store in memory
	candidatesMu.Lock()
	for _, c := range cands {
		candidates[c.ID] = c
	}
	candidatesMu.Unlock()

	writeJSON(w, cands)
}

// apiRecruitAccept accepts a candidate, creates the employee, and spawns a worker.
func (s *Server) apiRecruitAccept(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CandidateID string `json:"candidate_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}

	candidatesMu.Lock()
	cand, ok := candidates[input.CandidateID]
	if ok {
		delete(candidates, input.CandidateID)
	}
	candidatesMu.Unlock()

	if !ok {
		writeErr(w, 404, "candidate not found or already accepted")
		return
	}

	// Create employee
	emp, err := s.DB.CreateEmployee(cand.Name, "worker", cand.Persona, "")
	if err != nil {
		writeErr(w, 500, fmt.Sprintf("create employee: %v", err))
		return
	}
	s.DB.JoinChannel("general", emp.ID)

	// Spawn worker
	workerBin := s.WorkerBin
	if workerBin == "" {
		self, _ := os.Executable()
		if self != "" {
			candidate := filepath.Join(filepath.Dir(self), "cmd-worker")
			if _, err := os.Stat(candidate); err == nil {
				workerBin = candidate
			}
		}
	}
	if workerBin == "" {
		if p, err := exec.LookPath("cmd-worker"); err == nil {
			workerBin = p
		}
	}

	var spawnMsg string
	if workerBin != "" {
		home, _ := os.UserHomeDir()
		logsDir := filepath.Join(home, ".config", "artificial", "logs")
		os.MkdirAll(logsDir, 0755)
		logPath := filepath.Join(logsDir, fmt.Sprintf("worker-%s-%d.log", emp.Nickname, time.Now().Unix()))

		worker, _ := s.DB.CreateWorker(emp.ID, 0, logPath)

		serverAddr := fmt.Sprintf("localhost:%d", s.Port)
		cmd := exec.Command(workerBin,
			"--server", serverAddr,
			"--employee-id", strconv.FormatInt(emp.ID, 10),
			"--worker-id", strconv.FormatInt(worker.ID, 10),
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdin = nil
		logFile, logErr := os.Create(logPath)
		if logErr == nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
		if err := cmd.Start(); err != nil {
			spawnMsg = fmt.Sprintf("employee created but spawn failed: %v", err)
		} else {
			pid := cmd.Process.Pid
			cmd.Process.Release()
			if logFile != nil {
				logFile.Close()
			}
			s.DB.UpdateWorkerPID(worker.ID, pid)
			s.DB.UpdateWorkerStatus(worker.ID, "online")
			spawnMsg = fmt.Sprintf("hired and spawned %s (PID %d)", emp.Nickname, pid)
		}
	} else {
		spawnMsg = fmt.Sprintf("hired %s but no worker binary found", emp.Nickname)
	}

	writeJSON(w, map[string]any{
		"employee": emp,
		"message":  spawnMsg,
	})
}

func (s *Server) apiListReviews(w http.ResponseWriter, r *http.Request) {
	reviews, err := s.DB.ListPendingReviews()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if reviews == nil {
		reviews = []protocol.Review{}
	}
	writeJSON(w, reviews)
}

func (s *Server) apiRespondToReview(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var input struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	review, err := s.DB.RespondToReview(id, input.Response)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Notify the worker via WS
	data, _ := json.Marshal(review)
	s.Hub.sendTo(review.WorkerNick, protocol.WSMessage{
		Type: protocol.MsgReviewResponded,
		Data: data,
	})
	// For runner-originated reviews, also push the response into the
	// runner's channel-notification stream as a MsgWorkerNotify. The
	// runner's hub handler routes that into Claude as a directive,
	// which is the only way the ephemeral runner can hear back from
	// the manager (it has no chat surface).
	if strings.HasPrefix(review.Type, "runner_") {
		s.Hub.sendTo(review.WorkerNick, protocol.WSMessage{
			Type: protocol.MsgWorkerNotify,
			From: "manager",
			Text: input.Response,
		})
	}
	s.Hub.broadcast(protocol.WSMessage{
		Type: protocol.MsgReviewResponded,
		Data: data,
	}, "")
	writeJSON(w, review)
}

func (s *Server) apiServeFile(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		writeErr(w, 400, "path required")
		return
	}
	cleaned := filepath.Clean(reqPath)
	if strings.Contains(cleaned, "..") {
		writeErr(w, 403, "path traversal not allowed")
		return
	}
	http.ServeFile(w, r, cleaned)
}

func (s *Server) apiGetUnread(w http.ResponseWriter, r *http.Request) {
	empID := pathID(r, "employeeID")
	counts, err := s.DB.GetUnreadCounts(empID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, counts)
}

// apiStreamWorkerLogs streams a worker's slog output via SSE.
func (s *Server) apiStreamWorkerLogs(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	worker, err := s.DB.GetWorker(id)
	if err != nil {
		writeErr(w, 404, "worker not found")
		return
	}
	if worker.LogPath == "" {
		writeErr(w, 404, "no log file for this worker")
		return
	}
	s.streamFile(w, r, worker.LogPath)
}

// apiKillWorker sends SIGTERM to a worker process, then marks it offline.
func (s *Server) apiKillWorker(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	worker, err := s.DB.GetWorker(id)
	if err != nil {
		writeErr(w, 404, "worker not found")
		return
	}

	if worker.PID > 0 {
		proc, err := os.FindProcess(worker.PID)
		if err == nil {
			// Try SIGTERM first
			proc.Signal(syscall.SIGTERM)
		}
	}

	s.DB.UpdateWorkerStatus(id, "offline")
	if emp, err := s.DB.GetEmployee(worker.EmployeeID); err == nil {
		s.Hub.BroadcastWorkerStatus(emp.Nickname, "offline")
	}
	writeJSON(w, map[string]string{"status": "killed"})
}

// apiSetWorkerSession stores the Claude session ID on a worker (called by cmd-worker).
func (s *Server) apiSetWorkerSession(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var input struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err := s.DB.UpdateWorkerSessionID(id, input.SessionID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// apiSetWorkerTranscript stores the transcript path on a worker (called by cmd-worker).
func (s *Server) apiSetWorkerTranscript(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var input struct {
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err := s.DB.UpdateWorkerTranscriptPath(id, input.TranscriptPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// apiStreamWorkerTTY streams raw PTY output via SSE (base64-encoded chunks) for xterm.js.
func (s *Server) apiStreamWorkerTTY(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	worker, err := s.DB.GetWorker(id)
	if err != nil {
		writeErr(w, 404, "worker not found")
		return
	}
	// Find the .tty file: same log dir, named <nick>-<timestamp>.tty
	emp, _ := s.DB.GetEmployee(worker.EmployeeID)
	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".config", "artificial", "logs")

	entries, _ := os.ReadDir(logsDir)
	var ttyPath string
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if strings.HasPrefix(name, emp.Nickname+"-") && strings.HasSuffix(name, ".tty") {
			ttyPath = filepath.Join(logsDir, name)
			break
		}
	}
	if ttyPath == "" {
		writeErr(w, 404, "no TTY log found for this worker")
		return
	}

	f, err := os.Open(ttyPath)
	if err != nil {
		writeErr(w, 404, "tty file not found")
		return
	}
	defer f.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ctx := r.Context()
	tmp := make([]byte, 16*1024)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				n, _ := f.Read(tmp)
				if n == 0 {
					break
				}
				// Send raw bytes as base64 — xterm.js will decode
				encoded := base64.StdEncoding.EncodeToString(tmp[:n])
				fmt.Fprintf(w, "data: %s\n\n", encoded)
			}
			flusher.Flush()
		}
	}
}

// streamFile streams a file via SSE with raw lines.
func (s *Server) streamFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "file not found")
		return
	}
	defer f.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ctx := r.Context()
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				n, _ := f.Read(tmp)
				if n == 0 {
					break
				}
				buf = append(buf, tmp[:n]...)
				for {
					idx := bytes.IndexByte(buf, '\n')
					if idx < 0 {
						break
					}
					line := buf[:idx]
					buf = buf[idx+1:]
					if len(line) > 0 {
						fmt.Fprintf(w, "data: %s\n\n", line)
					}
				}
			}
			flusher.Flush()
		}
	}
}

// apiStreamWorkerTranscript streams a worker's Claude transcript JSONL via SSE.
func (s *Server) apiStreamWorkerTranscript(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	worker, err := s.DB.GetWorker(id)
	if err != nil {
		writeErr(w, 404, "worker not found")
		return
	}
	if worker.TranscriptPath == "" {
		writeErr(w, 404, "no transcript for this worker (session not yet discovered)")
		return
	}
	s.streamFile(w, r, worker.TranscriptPath)
}
