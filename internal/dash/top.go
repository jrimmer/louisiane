package dash

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jrimmer/louisiane/internal/grid"
	"github.com/jrimmer/louisiane/internal/proto"
)

// EventText is one event as a line of plain text.
func EventText(e proto.Event) string {
	var b strings.Builder
	for _, s := range eventLine(e)[1:] {
		b.WriteString(s.Text)
	}
	return b.String()
}

// Top redraws a view in the terminal every second until ctx ends. frame
// draws it at the terminal's width.
func Top(ctx context.Context, frame func(width int) (*grid.Grid, error)) error {
	styles := ANSIStyles()
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		g, err := frame(termWidth())
		if ctx.Err() != nil {
			return nil
		}
		out := "\x1b[H\x1b[2J"
		if err != nil {
			out += "lal top: " + err.Error() + "\n"
		} else {
			out += g.ANSI(styles) + "\n"
		}
		fmt.Print(out)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func termWidth() int {
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		return int(ws.Col)
	}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil {
		return n
	}
	return DefaultWidth
}
