package protocol

// EmployeeConfig is returned by GET /api/employees/:id to give a worker
// everything it needs to start: persona, channels, project path, etc.
type EmployeeConfig struct {
	Employee             Employee `json:"employee"`
	Channels             []string `json:"channels"`                        // channel names this employee is a member of
	Project              *Project `json:"project,omitempty"`                // assigned project (if any)
	Returning            bool     `json:"returning"`                        // true if this employee has had a previous worker
	PreviousSessionID    string   `json:"previous_session_id,omitempty"`    // last Claude session ID for resume
	CompanyKnowledgePath string   `json:"company_knowledge_path,omitempty"` // path to company knowledge directory
}

// DMChannelName returns the canonical DM channel name for two nicknames.
// Nicks are sorted alphabetically so dm:alice:bob == dm:bob:alice.
func DMChannelName(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}
