// Package waitpage embeds and renders the self-contained waiting-room page.
package waitpage

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"time"
)

//go:embed wait.html
var pageTmpl string

// Render produces the waiting page HTML once at startup; the page then keeps
// itself up to date by polling /gowait/status.
func Render(pollInterval time.Duration) ([]byte, error) {
	t, err := template.New("wait").Parse(pageTmpl)
	if err != nil {
		return nil, fmt.Errorf("parsing wait page template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, struct{ PollMs int64 }{pollInterval.Milliseconds()}); err != nil {
		return nil, fmt.Errorf("rendering wait page: %w", err)
	}
	return buf.Bytes(), nil
}
