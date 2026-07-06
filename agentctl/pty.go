package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// spawnClaude launches `claude` in a PTY under s.Cwd, pinned to s.SessionID.
// PTY output is teed into the ring buffer and broadcast to WS subscribers.
func spawnClaude(s *Session, settingsFile, prompt string) error {
	args := []string{"--session-id", s.SessionID, "--settings", settingsFile}
	if prompt != "" {
		args = append(args, prompt)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = s.Cwd
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"AGENTCTL_AGENT_ID="+s.ID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	s.Cmd = cmd
	s.PTY = ptmx

	// Default a reasonable window size; xterm.js sends resize elsewhere if we add it later.
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})

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
