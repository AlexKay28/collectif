package main

import (
	"fmt"
	"io"
	"log"

	"github.com/creack/pty"
)

// spawnSession launches the session's CLI in a PTY under s.Cwd. The choice
// of CLI is driven by s.CLI (defaulting to "claude") via the CLIAdapter
// registry — this file is CLI-agnostic in Phase 1 of #46. Adapters own the
// exec.Cmd construction (binary, args, env, settings file) and return a
// cleanup we invoke on session teardown.
//
// PTY output is teed into the ring buffer and broadcast to WS subscribers.
func spawnSession(s *Session, hookURL string) error {
	adapter := s.adapter()
	if adapter == nil {
		return fmt.Errorf("unknown cli: %q", s.CLI)
	}

	cmd, cleanup, err := adapter.Spawn(SpawnRequest{
		SessionID: s.SessionID,
		Cwd:       s.Cwd,
		Prompt:    s.Prompt,
		HookURL:   hookURL,
		AgentID:   s.ID,
	})
	if err != nil {
		return err
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		cleanup()
		return err
	}
	// Publish Cmd/PTY under s.mu so readers via the pty()/cmd() accessors
	// see a fully initialized handle (issue #3). Release before spawning
	// the reader goroutine — cmd.Wait blocks and we don't want to hold s.mu.
	s.mu.Lock()
	s.Cmd = cmd
	s.PTY = ptmx
	s.spawnCleanup = cleanup
	s.mu.Unlock()

	// Default a reasonable window size; xterm.js sends resize elsewhere if we add it later.
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})

	// Start the numbered-menu detector — polls the ring buffer every 250ms
	// and publishes MenuOptions to the session state so the UI can render
	// clickable buttons for any TUI selection the CLI opens.
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
