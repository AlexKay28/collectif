package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiBlue    = "\x1b[34m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
	ansiGray    = "\x1b[90m"
)

// setupLogging routes stdlib log through a colorizer so every existing
// log.Printf call — plus any future ones — comes out formatted the same
// way. No code change needed at the call sites.
//
// Format (colorized only when stderr is a TTY):
//     15:04:05.123  INFO  [agent-8char] message body key=value
//
// Level is inferred from the message content: keywords like "error", non-nil
// "err=" values, or the "warn:"/"error:" prefixes bump the color.
func setupLogging() {
	stat, _ := os.Stderr.Stat()
	tty := stat != nil && stat.Mode()&os.ModeCharDevice != 0
	if os.Getenv("NO_COLOR") != "" {
		tty = false
	}
	log.SetFlags(0)
	log.SetOutput(&colorLogger{out: os.Stderr, tty: tty})
}

type colorLogger struct {
	out io.Writer
	tty bool
	mu  sync.Mutex
}

// Strips stdlib LstdFlags prefix in case something wrote through a logger
// that still carries it.
var stdlibDatePrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? `)

// Detects a leading "[uuid-or-tag]" so we can pull the agent identifier
// into a colored badge separate from the message.
var leadingTag = regexp.MustCompile(`^\[([0-9a-fA-F][0-9a-fA-F-]{5,})\]\s*`)

func (c *colorLogger) Write(p []byte) (int, error) {
	orig := len(p)
	line := strings.TrimRight(string(p), "\n")
	line = stdlibDatePrefix.ReplaceAllString(line, "")

	level := inferLevel(line)

	tag := ""
	if m := leadingTag.FindStringSubmatch(line); m != nil {
		tag = m[1]
		line = line[len(m[0]):]
	}
	if len(tag) > 8 {
		tag = tag[:8]
	}

	var b strings.Builder
	ts := time.Now().Format("15:04:05.000")

	if c.tty {
		b.WriteString(ansiGray)
		b.WriteString(ts)
		b.WriteString(ansiReset)
		b.WriteString("  ")
		b.WriteString(levelColor(level))
		b.WriteString(fmt.Sprintf("%-5s", strings.ToUpper(level)))
		b.WriteString(ansiReset)
		b.WriteString("  ")
		if tag != "" {
			b.WriteString(ansiBlue)
			b.WriteString("[" + tag + "]")
			b.WriteString(ansiReset)
			b.WriteString(" ")
		}
		b.WriteString(colorizeMessage(line))
	} else {
		b.WriteString(ts)
		b.WriteString("  ")
		b.WriteString(fmt.Sprintf("%-5s", strings.ToUpper(level)))
		b.WriteString("  ")
		if tag != "" {
			b.WriteString("[" + tag + "] ")
		}
		b.WriteString(line)
	}
	b.WriteByte('\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.out.Write([]byte(b.String())); err != nil {
		return 0, err
	}
	return orig, nil
}

func inferLevel(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.HasPrefix(l, "fatal") || strings.HasPrefix(l, "error:") || strings.HasPrefix(l, "error "):
		return "error"
	case strings.HasPrefix(l, "warn:") || strings.HasPrefix(l, "warning:") || strings.HasPrefix(l, "warn "):
		return "warn"
	case strings.HasPrefix(l, "debug:") || strings.HasPrefix(l, "debug "):
		return "debug"
	case strings.Contains(l, "input/output error"),
		strings.Contains(l, "pty read"),
		strings.Contains(l, "pty answer chunk"),
		strings.Contains(l, "pty input via"):
		return "debug"
	case strings.Contains(l, "err=") && !strings.Contains(l, "err=<nil>"),
		strings.Contains(l, "failed"),
		strings.Contains(l, "refused"):
		return "warn"
	}
	return "info"
}

func levelColor(level string) string {
	switch level {
	case "debug":
		return ansiCyan
	case "warn":
		return ansiYellow
	case "error":
		return ansiRed + ansiBold
	default:
		return ansiGreen
	}
}

// Highlights common structural noise in the message body: key= values in dim
// gray, hex/UUID substrings in cyan. Purely cosmetic; falls back to plain
// text on non-TTY.
var kvKey = regexp.MustCompile(`(\b[a-zA-Z_][a-zA-Z0-9_.]*)=`)

func colorizeMessage(s string) string {
	return kvKey.ReplaceAllString(s, ansiGray+"$1"+ansiReset+"=")
}
