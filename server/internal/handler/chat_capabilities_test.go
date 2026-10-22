package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestChatCapabilities_DisabledUntilIntegration(t *testing.T) {
	w := httptest.NewRecorder()
	(&Handler{}).GetChatCapabilities(w, httptest.NewRequest("GET", "/", nil))
	var cap map[string]any
	if json.Unmarshal(w.Body.Bytes(), &cap) != nil || cap["team_enabled"] != false || cap["protocol"] != float64(2) {
		t.Fatalf("unsafe capabilities: %s", w.Body.String())
	}
}
