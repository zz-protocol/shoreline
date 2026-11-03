package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// T36：docs/AGENT_GUIDE.md 与嵌入副本一致；必备章节齐全；REST（无需认证）与 MCP get_agent_guide 返回同一内容。
func TestT36AgentGuide(t *testing.T) {
	doc, err := os.ReadFile("../docs/AGENT_GUIDE.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, agentGuideMD) {
		t.Fatal("backend/agent_guide.md 与 docs/AGENT_GUIDE.md 不一致：请 cp docs/AGENT_GUIDE.md backend/agent_guide.md")
	}
	for _, s := range []string{"Authorization: Bearer", "/api/mine/claim", "/api/mine/heartbeat", "/api/mine/complete",
		"claim_mine_targets", "heartbeat_mine", "complete_mine", "get_agent_guide",
		"claimed", "done", "deferred", "abandoned", "MINE_CLAIM_TTL_HOURS", "MINE_COOLDOWN_DAYS", "| 24 |", "| 30 |",
		"| 400 |", "| 401 |", "| 409 |", "system prompt", "必须调用 complete_mine"} {
		if !strings.Contains(string(doc), s) {
			t.Fatalf("guide missing %q", s)
		}
	}
	_, c, _ := newAPITest(t, 100)
	resp, err := http.Get(c.srv.URL + "/api/agent-guide") // 无 token
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") || !bytes.Equal(body, doc) {
		t.Fatalf("GET /api/agent-guide: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callTool(t, s, "get_agent_guide", map[string]any{})
	if isErr || out["guide"] != string(doc) {
		t.Fatalf("get_agent_guide isErr=%v len=%d", isErr, len(out["guide"].(string)))
	}
	t.Logf("guide %d 字节：docs 与嵌入副本一致，REST 无需认证、MCP get_agent_guide 返回同一内容", len(doc))
}
