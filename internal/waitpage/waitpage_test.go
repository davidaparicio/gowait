package waitpage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderDefaults(t *testing.T) {
	html, err := Render(Options{PollInterval: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{
		`<html lang="en">`,
		"<title>You are in the waiting room</title>",
		"You are in the waiting room",
		"Your estimated wait time",
		"var pollMs =  3000 ;", // the JS escaper pads interpolated values

		`"Position {pos} of {len} in the queue"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(page, `class="brand"`) {
		t.Error("brand block rendered without a brand")
	}
}

func TestRenderFrench(t *testing.T) {
	html, err := Render(Options{PollInterval: time.Second, Lang: "fr"})
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{
		`<html lang="fr">`,
		"Vous êtes dans la salle d&#39;attente",
		"Votre temps d&#39;attente estimé",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestRenderCustomTexts(t *testing.T) {
	html, err := Render(Options{
		PollInterval: time.Second,
		Title:        "High demand",
		Brand:        "ACME <shop>",
		Message:      "Hang tight.",
	})
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{
		"<title>ACME &lt;shop&gt; — High demand</title>",
		"<h1>High demand</h1>",
		"Hang tight.",
		`<div class="brand">ACME &lt;shop&gt;</div>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(page, "<shop>") {
		t.Error("brand not HTML-escaped")
	}
}

func TestRenderUnsupportedLang(t *testing.T) {
	if _, err := Render(Options{PollInterval: time.Second, Lang: "de"}); err == nil {
		t.Fatal("expected error for unsupported language")
	}
}

func TestRenderCustomTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wait.html")
	tmpl := `<p>{{.Brand}} says {{.L.EtaLabel}}, poll {{.PollMs}}ms</p>`
	if err := os.WriteFile(path, []byte(tmpl), 0o600); err != nil {
		t.Fatal(err)
	}
	html, err := Render(Options{PollInterval: 2 * time.Second, Lang: "fr", Brand: "ACME", TemplatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	want := "<p>ACME says Votre temps d&#39;attente estimé, poll 2000ms</p>"
	if got := string(html); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderTemplateErrors(t *testing.T) {
	if _, err := Render(Options{PollInterval: time.Second, TemplatePath: "/does/not/exist.html"}); err == nil {
		t.Error("expected error for missing template file")
	}

	path := filepath.Join(t.TempDir(), "bad.html")
	if err := os.WriteFile(path, []byte("{{.Unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(Options{PollInterval: time.Second, TemplatePath: path}); err == nil {
		t.Error("expected error for unparsable template")
	}
}
