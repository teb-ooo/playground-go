package mail

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

// The base layouts are owned by @teb-ooo/ui (its email/ directory) and embedded
// here as byte-identical copies. These tests fail if a copy stops honouring the
// placeholder contract, and, when FACTORY_UI_EMAIL_DIR points at the ui
// package's email/ directory, if the copies diverge from it.

var placeholders = []string{"{{.Title}}", "{{.Preheader}}", "{{.FactoryName}}", "{{.Footer}}", `{{template "content" .}}`}

func TestBaseTemplatesHonourPlaceholderContract(t *testing.T) {
	for _, name := range []string{"templates/base.html.tmpl", "templates/base.txt.tmpl"} {
		t.Run(name, func(t *testing.T) {
			b, err := baseFS.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			src := string(b)
			for _, p := range placeholders {
				if name == "templates/base.txt.tmpl" && p == "{{.Preheader}}" {
					continue // preheader is inbox preview text: HTML only
				}
				if !strings.Contains(src, p) {
					t.Errorf("%s lacks %s", name, p)
				}
			}
			// The only actions allowed are the contract's.
			for _, act := range regexp.MustCompile(`\{\{[^}]*\}\}`).FindAllString(src, -1) {
				ok := false
				for _, p := range placeholders {
					if act == p {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s uses %s, which is outside the placeholder contract", name, act)
				}
			}
			if name == "templates/base.html.tmpl" && !strings.Contains(strings.ToLower(src), "<!doctype html>") {
				t.Error("HTML base is not a complete document")
			}
		})
	}
}

func TestBaseTemplatesRenderWithSampleContent(t *testing.T) {
	if _, err := NewTemplates(nil, "teb.ooo"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tt := template.Must(template.New("b").ParseFS(baseFS, "templates/base.txt.tmpl"))
	template.Must(tt.New("c").Parse(`{{define "content"}}BODY{{end}}`))
	if err := tt.ExecuteTemplate(&buf, "base.txt.tmpl", Page{Title: "T", FactoryName: "F", Footer: "Foot"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"T", "F", "BODY", "Foot"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("rendered text base lacks %q: %q", want, buf.String())
		}
	}
}

func TestEmbeddedCopiesMatchUIPackage(t *testing.T) {
	dir := os.Getenv("FACTORY_UI_EMAIL_DIR")
	if dir == "" {
		t.Skip("FACTORY_UI_EMAIL_DIR not set (point it at lib/ui/email to compare)")
	}
	for _, name := range []string{"base.html.tmpl", "base.txt.tmpl"} {
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := baseFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("mail/templates/%s differs from %s: copy it over byte for byte", name, filepath.Join(dir, name))
		}
	}
}
