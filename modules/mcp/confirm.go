//go:build !no_mcp

package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvRequireHumanConfirm makes destructive confirm tokens valid only after a
// human operator approved them on the CLI with `toki agent confirm <token>`.
// Without it the token is a speed bump: it is handed to the same agent that
// asked for it.
const EnvRequireHumanConfirm = "TOKI_MCP_REQUIRE_HUMAN_CONFIRM"

func requireHumanConfirm() bool { return os.Getenv(EnvRequireHumanConfirm) == "1" }

// pendingConfirm is the on-disk state of a plan awaiting human approval.
type pendingConfirm struct {
	Agent    string    `json:"agent"`
	AgentID  string    `json:"agent_id"`
	Tool     string    `json:"tool"`
	Summary  string    `json:"summary"`
	Expires  time.Time `json:"expires"`
	Approved bool      `json:"approved"`
}

func confirmPath(dataDir, token string) string {
	sum := sha256.Sum256([]byte(token))
	return filepath.Join(dataDir, ".mcp_confirm", hex.EncodeToString(sum[:16])+".json")
}

func writePending(dataDir, token string, p *pendingConfirm) error {
	path := confirmPath(dataDir, token)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func readPending(dataDir, token string) (*pendingConfirm, error) {
	raw, err := os.ReadFile(confirmPath(dataDir, token))
	if err != nil {
		return nil, errors.New("unknown confirm token")
	}
	p := &pendingConfirm{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, errors.New("unknown confirm token")
	}
	return p, nil
}

// ApproveConfirm marks a pending plan as approved by a human (CLI only).
func ApproveConfirm(dataDir, token string, now time.Time) (*pendingConfirm, error) {
	token = strings.TrimSpace(token)
	p, err := readPending(dataDir, token)
	if err != nil {
		return nil, err
	}
	if now.After(p.Expires) {
		_ = os.Remove(confirmPath(dataDir, token))
		return nil, errors.New("confirm token expired")
	}
	p.Approved = true
	if err := writePending(dataDir, token, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Server) nextHint(tool string) string {
	if requireHumanConfirm() {
		return "a human operator must approve this plan: run `toki agent confirm <confirm_token>` on the CLI, then call " + tool + " again with identical arguments plus confirm_token"
	}
	return "call " + tool + " again with identical arguments plus confirm_token to execute"
}

func planSummary(args any) string {
	raw, _ := json.Marshal(sanitize(jsonValue(args), 60))
	r := []rune(string(raw))
	if len(r) > 400 {
		return string(r[:400]) + "…"
	}
	return string(r)
}

func approvalErr(token string) error {
	return fmt.Errorf("this plan needs human approval: an operator must run `toki agent confirm %s` on the CLI, then retry", token)
}
