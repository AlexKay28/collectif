package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// writeHookSettings creates a scratch dir containing a .claude/settings.json
// with HTTP hooks pointing at our /api/hooks endpoint. Claude is launched with
// --settings <file> so these override any project/user settings.
func writeHookSettings(hookURL string) (settingsDir, settingsFile string, err error) {
	dir, err := os.MkdirTemp("", "agentctl-settings-")
	if err != nil {
		return "", "", err
	}

	hook := func(event string) map[string]any {
		return map[string]any{
			"matcher": "*",
			"hooks": []map[string]any{
				{"type": "http", "url": hookURL, "timeout": 5},
			},
		}
	}
	notifHook := func(matcher string) map[string]any {
		return map[string]any{
			"matcher": matcher,
			"hooks": []map[string]any{
				{"type": "http", "url": hookURL, "timeout": 5},
			},
		}
	}

	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart":       []map[string]any{hook("SessionStart")},
			"SessionEnd":         []map[string]any{hook("SessionEnd")},
			"UserPromptSubmit":   []map[string]any{hook("UserPromptSubmit")},
			"PreToolUse":         []map[string]any{hook("PreToolUse")},
			"PostToolUse":        []map[string]any{hook("PostToolUse")},
			"PostToolUseFailure": []map[string]any{hook("PostToolUseFailure")},
			"Stop":               []map[string]any{hook("Stop")},
			"Notification": []map[string]any{
				notifHook("permission_prompt"),
				notifHook("idle_prompt"),
				notifHook("*"),
			},
		},
	}

	f := filepath.Join(dir, "settings.json")
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if err := os.WriteFile(f, b, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, f, nil
}

func hookURL(bind, port string) string {
	return fmt.Sprintf("http://%s:%s/api/hooks", bind, port)
}
