// Package waitpage embeds and renders the self-contained waiting-room page.
package waitpage

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"os"
	"sort"
	"strings"
	"time"
)

//go:embed wait.html
var pageTmpl string

// Options customizes the waiting page. Zero values fall back to the built-in
// English page.
type Options struct {
	PollInterval time.Duration
	Lang         string // page language: "en" (default) or "fr"
	Title        string // overrides the localized title/heading
	Brand        string // optional brand name shown above the heading
	Message      string // overrides the localized explanation paragraph
	TemplatePath string // optional file overriding the embedded template
}

// Strings holds the localized fixed texts of the page. {pos}, {len} and
// {time} are placeholders substituted by the page's script.
type Strings struct {
	Title      string
	Message    string
	EtaLabel   string
	Notice     string
	LessMinute string
	Minute     string
	Minutes    string
	Position   string
	Updated    string
}

var locales = map[string]Strings{
	"en": {
		Title:      "You are in the waiting room",
		Message:    "Due to a high number of simultaneous connections, we invite you to wait in the queue. You will be let in automatically.",
		EtaLabel:   "Your estimated wait time",
		Notice:     "Keep this page open. You will be redirected automatically to the service as soon as possible. Refreshing will not lose your place in line.",
		LessMinute: "< 1 minute",
		Minute:     "minute",
		Minutes:    "minutes",
		Position:   "Position {pos} of {len} in the queue",
		Updated:    "Last updated at {time}",
	},
	"fr": {
		Title:      "Vous êtes dans la salle d'attente",
		Message:    "En raison d'un grand nombre de connexions simultanées, nous vous invitons à patienter dans la file. Vous serez admis automatiquement.",
		EtaLabel:   "Votre temps d'attente estimé",
		Notice:     "Gardez cette page ouverte. Vous serez redirigé automatiquement vers le service dès que possible. Actualiser ne vous fera pas perdre votre place dans la file.",
		LessMinute: "< 1 minute",
		Minute:     "minute",
		Minutes:    "minutes",
		Position:   "Position {pos} sur {len} dans la file d'attente",
		Updated:    "Dernière mise à jour à {time}",
	},
}

// Langs returns the supported language codes, sorted.
func Langs() []string {
	langs := make([]string, 0, len(locales))
	for l := range locales {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	return langs
}

// SupportedLang reports whether lang has a built-in translation.
func SupportedLang(lang string) bool {
	_, ok := locales[lang]
	return ok
}

// Data is what the page template (embedded or user-provided via
// Options.TemplatePath) is executed with.
type Data struct {
	PollMs  int64
	Lang    string
	Title   string
	Brand   string
	Message string
	L       Strings
}

// Render produces the waiting page HTML once at startup; the page then keeps
// itself up to date by polling /gowait/status.
func Render(opts Options) ([]byte, error) {
	lang := opts.Lang
	if lang == "" {
		lang = "en"
	}
	loc, ok := locales[lang]
	if !ok {
		return nil, fmt.Errorf("unsupported wait page language %q (supported: %s)", lang, strings.Join(Langs(), ", "))
	}

	src := pageTmpl
	if opts.TemplatePath != "" {
		b, err := os.ReadFile(opts.TemplatePath)
		if err != nil {
			return nil, fmt.Errorf("reading wait page template: %w", err)
		}
		src = string(b)
	}

	data := Data{
		PollMs:  opts.PollInterval.Milliseconds(),
		Lang:    lang,
		Title:   opts.Title,
		Brand:   opts.Brand,
		Message: opts.Message,
		L:       loc,
	}
	if data.Title == "" {
		data.Title = loc.Title
	}
	if data.Message == "" {
		data.Message = loc.Message
	}

	t, err := template.New("wait").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parsing wait page template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering wait page: %w", err)
	}
	return buf.Bytes(), nil
}
