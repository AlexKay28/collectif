package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

// prependPATH puts dir at the front of PATH in env. Idempotent — if PATH
// already begins with dir, env is returned unchanged.
func prependPATH(env []string, dir string) []string {
	for i, e := range env {
		if !strings.HasPrefix(e, "PATH=") {
			continue
		}
		cur := strings.TrimPrefix(e, "PATH=")
		if strings.HasPrefix(cur, dir+":") || cur == dir {
			return env
		}
		env[i] = "PATH=" + dir + ":" + cur
		return env
	}
	return append(env, "PATH="+dir)
}

// collectifBinDir returns the directory containing the running collectif
// binary, so spawned children can find `collectif` on their PATH. Determined
// once at startup.
var collectifBinDir = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}()

// spawnClaude launches `claude` in a PTY under s.Cwd, pinned to s.SessionID.
// PTY output is teed into the ring buffer and broadcast to WS subscribers.
func spawnClaude(s *Session, settingsFile, prompt string) error {
	args := []string{"--session-id", s.SessionID, "--settings", settingsFile}
	if prompt != "" {
		args = append(args, prompt)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = s.Cwd
	env := append(os.Environ(),
		"TERM=xterm-256color",
		"AGENTCTL_AGENT_ID="+s.ID,
		"COLLECTIF_AGENT_ID="+s.ID,
		"COLLECTIF_PARENT_ID="+s.ParentID,
		"COLLECTIF_TOKEN="+authToken,
		"COLLECTIF_URL=http://"+hookBind+":"+hookPort,
	)
	if collectifBinDir != "" {
		env = prependPATH(env, collectifBinDir)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	s.Cmd = cmd
	s.PTY = ptmx

	// Default a reasonable window size; xterm.js sends resize elsewhere if we add it later.
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})

	// Start the numbered-menu detector — polls the ring buffer every 250ms
	// and publishes MenuOptions to the session state so the UI can render
	// clickable buttons for any TUI selection Claude opens.
	startMenuDetector(s.ctx, s)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				s.writeRing(chunk)
				s.broadcastBytes(chunk)
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[%s] pty read: %v", s.ID, err)
				}
				break
			}
		}
		waitErr := cmd.Wait()
		status := "stopped"
		activity := ""
		if waitErr != nil {
			status = "error"
			activity = waitErr.Error()
		}
		s.setStatus(status, activity)
		s.closeSubs()
		_ = ptmx.Close()
	}()

	return nil
}
